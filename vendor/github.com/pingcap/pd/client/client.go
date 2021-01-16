// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package pd

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	"github.com/pingcap/pd/pkg/grpcutil"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// Client is a PD (Placement Driver) client.
// It should not be used after calling Close().
type Client interface {
	// GetClusterID gets the cluster ID from PD.
	GetClusterID(ctx context.Context) uint64
	// GetTS gets a timestamp from PD.
	GetTS(ctx context.Context) (int64, int64, error)
	// GetTSAsync gets a timestamp from PD, without block the caller.
	GetTSAsync(ctx context.Context) TSFuture
	// GetRegion gets a region and its leader Peer from PD by key.
	// The region may expire after split. Caller is responsible for caching and
	// taking care of region change.
	// Also it may return nil if PD finds no Region for the key temporarily,
	// client should retry later.
	GetRegion(ctx context.Context, key []byte) (*metapb.Region, *metapb.Peer, error)
	// GetPrevRegion gets the previous region and its leader Peer of the region where the key is located.
	GetPrevRegion(ctx context.Context, key []byte) (*metapb.Region, *metapb.Peer, error)
	// GetRegionByID gets a region and its leader Peer from PD by id.
	GetRegionByID(ctx context.Context, regionID uint64) (*metapb.Region, *metapb.Peer, error)
	// ScanRegion gets a list of regions, starts from the region that contains key.
	// Limit limits the maximum number of regions returned.
	// If a region has no leader, corresponding leader will be placed by a peer
	// with empty value (PeerID is 0).
	ScanRegions(ctx context.Context, key []byte, limit int) ([]*metapb.Region, []*metapb.Peer, error)
	// GetStore gets a store from PD by store id.
	// The store may expire later. Caller is responsible for caching and taking care
	// of store change.
	GetStore(ctx context.Context, storeID uint64) (*metapb.Store, error)
	// GetAllStores gets all stores from pd.
	// The store may expire later. Caller is responsible for caching and taking care
	// of store change.
	GetAllStores(ctx context.Context, opts ...GetStoreOption) ([]*metapb.Store, error)
	// Update GC safe point. TiKV will check it and do GC themselves if necessary.
	// If the given safePoint is less than the current one, it will not be updated.
	// Returns the new safePoint after updating.
	UpdateGCSafePoint(ctx context.Context, safePoint uint64) (uint64, error)
	// ScatterRegion scatters the specified region. Should use it for a batch of regions,
	// and the distribution of these regions will be dispersed.
	ScatterRegion(ctx context.Context, regionID uint64) error
	// GetOperator gets the status of operator of the specified region.
	GetOperator(ctx context.Context, regionID uint64) (*pdpb.GetOperatorResponse, error)
	// Close closes the client.
	Close()
}

// GetStoreOp represents available options when getting stores.
type GetStoreOp struct {
	excludeTombstone bool
}

// GetStoreOption configures GetStoreOp.
type GetStoreOption func(*GetStoreOp)

// WithExcludeTombstone excludes tombstone stores from the result.
func WithExcludeTombstone() GetStoreOption {
	return func(op *GetStoreOp) { op.excludeTombstone = true }
}

type tsoRequest struct {
	start    time.Time
	ctx      context.Context
	done     chan error
	physical int64
	logical  int64
}

const (
	pdTimeout             = 3 * time.Second
	updateLeaderTimeout   = time.Second // Use a shorter timeout to recover faster from network isolation.
	maxMergeTSORequests   = 10000
	maxInitClusterRetries = 100
)

var (
	// errFailInitClusterID is returned when failed to load clusterID from all supplied PD addresses.
	errFailInitClusterID = errors.New("[pd] failed to get cluster id")
	// errClosing is returned when request is canceled when client is closing.
	errClosing = errors.New("[pd] closing")
	// errTSOLength is returned when the number of response timestamps is inconsistent with request.
	errTSOLength = errors.New("[pd] tso length in rpc response is incorrect")
)

type client struct {
	urls        []string
	clusterID   uint64
	tsoRequests chan *tsoRequest

	connMu struct {
		sync.RWMutex
		clientConns map[string]*grpc.ClientConn
		leader      string
	}

	tsDeadlineCh  chan deadline
	checkLeaderCh chan struct{}

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc

	security        SecurityOption
	gRPCDialOptions []grpc.DialOption
}

// SecurityOption records options about tls
type SecurityOption struct {
	CAPath   string
	CertPath string
	KeyPath  string
}

// ClientOption configures client.
type ClientOption func(c *client)

// WithGRPCDialOptions configures the client with gRPC dial options.
func WithGRPCDialOptions(opts ...grpc.DialOption) ClientOption {
	return func(c *client) {
		c.gRPCDialOptions = append(c.gRPCDialOptions, opts...)
	}
}

// NewClient creates a PD client.
func NewClient(pdAddrs []string, security SecurityOption, opts ...ClientOption) (Client, error) {
	return NewClientWithContext(context.Background(), pdAddrs, security, opts...)
}

// NewClientWithContext creates a PD client with context.
func NewClientWithContext(ctx context.Context, pdAddrs []string, security SecurityOption, opts ...ClientOption) (Client, error) {
	log.Info("[pd] create pd client with endpoints", zap.Strings("pd-address", pdAddrs))
	ctx1, cancel := context.WithCancel(ctx)
	c := &client{
		urls:          addrsToUrls(pdAddrs),
		tsoRequests:   make(chan *tsoRequest, maxMergeTSORequests),
		tsDeadlineCh:  make(chan deadline, 1),
		checkLeaderCh: make(chan struct{}, 1),
		ctx:           ctx1,
		cancel:        cancel,
		security:      security,
	}
	c.connMu.clientConns = make(map[string]*grpc.ClientConn)
	for _, opt := range opts {
		opt(c)
	}

	if err := c.initRetry(c.initClusterID); err != nil {
		cancel()
		return nil, err
	}
	if err := c.initRetry(c.updateLeader); err != nil {
		cancel()
		return nil, err
	}
	log.Info("[pd] init cluster id", zap.Uint64("cluster-id", c.clusterID))

	c.wg.Add(3)
	go c.tsLoop()
	go c.tsCancelLoop()
	go c.leaderLoop()

	return c, nil
}

func (c *client) updateURLs(members []*pdpb.Member) {
	urls := make([]string, 0, len(members))
	for _, m := range members {
		urls = append(urls, m.GetClientUrls()...)
	}

	sort.Strings(urls)
	// the url list is same.
	if reflect.DeepEqual(c.urls, urls) {
		return
	}

	log.Info("[pd] update member urls", zap.Strings("old-urls", c.urls), zap.Strings("new-urls", urls))
	c.urls = urls
}

func (c *client) initRetry(f func() error) error {
	var err error
	for i := 0; i < maxInitClusterRetries; i++ {
		if err = f(); err == nil {
			return nil
		}
		select {
		case <-c.ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
	return errors.WithStack(err)
}

func (c *client) initClusterID() error {
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	for _, u := range c.urls {
		timeoutCtx, timeoutCancel := context.WithTimeout(ctx, pdTimeout)
		members, err := c.getMembers(timeoutCtx, u)
		timeoutCancel()
		if err != nil || members.GetHeader() == nil {
			log.Warn("[pd] failed to get cluster id", zap.String("url", u), zap.Error(err))
			continue
		}
		c.clusterID = members.GetHeader().GetClusterId()
		return nil
	}
	return errors.WithStack(errFailInitClusterID)
}

func (c *client) updateLeader() error {
	for _, u := range c.urls {
		ctx, cancel := context.WithTimeout(c.ctx, updateLeaderTimeout)
		members, err := c.getMembers(ctx, u)
		if err != nil {
			log.Warn("[pd] cannot update leader", zap.String("address", u), zap.Error(err))
		}
		cancel()
		if err != nil || members.GetLeader() == nil || len(members.GetLeader().GetClientUrls()) == 0 {
			select {
			case <-c.ctx.Done():
				return errors.WithStack(err)
			default:
				continue
			}
		}
		c.updateURLs(members.GetMembers())
		return c.switchLeader(members.GetLeader().GetClientUrls())
	}
	return errors.Errorf("failed to get leader from %v", c.urls)
}

func (c *client) getMembers(ctx context.Context, url string) (*pdpb.GetMembersResponse, error) {
	cc, err := c.getOrCreateGRPCConn(url)
	if err != nil {
		return nil, err
	}
	members, err := pdpb.NewPDClient(cc).GetMembers(ctx, &pdpb.GetMembersRequest{})
	if err != nil {
		attachErr := errors.Errorf("error:%s target:%s status:%s", err, cc.Target(), cc.GetState().String())
		return nil, errors.WithStack(attachErr)
	}
	return members, nil
}

func (c *client) switchLeader(addrs []string) error {
	// FIXME: How to safely compare leader urls? For now, only allows one client url.
	addr := addrs[0]

	c.connMu.RLock()
	oldLeader := c.connMu.leader
	c.connMu.RUnlock()

	if addr == oldLeader {
		return nil
	}

	log.Info("[pd] switch leader", zap.String("new-leader", addr), zap.String("old-leader", oldLeader))
	if _, err := c.getOrCreateGRPCConn(addr); err != nil {
		return err
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()
	c.connMu.leader = addr
	return nil
}

func (c *client) getOrCreateGRPCConn(addr string) (*grpc.ClientConn, error) {
	c.connMu.RLock()
	conn, ok := c.connMu.clientConns[addr]
	c.connMu.RUnlock()
	if ok {
		return conn, nil
	}

	cc, err := grpcutil.GetClientConn(addr, c.security.CAPath, c.security.CertPath, c.security.KeyPath, c.gRPCDialOptions...)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if old, ok := c.connMu.clientConns[addr]; ok {
		cc.Close()
		log.Debug("use old connection", zap.String("target", cc.Target()), zap.String("state", cc.GetState().String()))
		return old, nil
	}

	c.connMu.clientConns[addr] = cc
	return cc, nil
}

func (c *client) leaderLoop() {
	defer c.wg.Done()

	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()

	for {
		select {
		case <-c.checkLeaderCh:
		case <-time.After(time.Minute):
		case <-ctx.Done():
			return
		}

		if err := c.updateLeader(); err != nil {
			log.Error("[pd] failed updateLeader", zap.Error(err))
		}
	}
}

type deadline struct {
	timer  <-chan time.Time
	done   chan struct{}
	cancel context.CancelFunc
}

func (c *client) tsCancelLoop() {
	defer c.wg.Done()

	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()

	for {
		select {
		case d := <-c.tsDeadlineCh:
			select {
			case <-d.timer:
				log.Error("tso request is canceled due to timeout")
				d.cancel()
			case <-d.done:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *client) tsLoop() {
	defer c.wg.Done()

	loopCtx, loopCancel := context.WithCancel(c.ctx)
	defer loopCancel()

	var requests []*tsoRequest
	var opts []opentracing.StartSpanOption
	var stream pdpb.PD_TsoClient
	var cancel context.CancelFunc

	for {
		var err error

		if stream == nil {
			var ctx context.Context
			ctx, cancel = context.WithCancel(loopCtx)
			stream, err = c.leaderClient().Tso(ctx)
			if err != nil {
				select {
				case <-loopCtx.Done():
					cancel()
					return
				default:
				}
				log.Error("[pd] create tso stream error", zap.Error(err))
				c.ScheduleCheckLeader()
				cancel()
				c.revokeTSORequest(errors.WithStack(err))
				select {
				case <-time.After(time.Second):
				case <-loopCtx.Done():
					return
				}
				continue
			}
		}

		select {
		case first := <-c.tsoRequests:
			requests = append(requests, first)
			pending := len(c.tsoRequests)
			for i := 0; i < pending; i++ {
				requests = append(requests, <-c.tsoRequests)
			}
			done := make(chan struct{})
			dl := deadline{
				timer:  time.After(pdTimeout),
				done:   done,
				cancel: cancel,
			}
			select {
			case c.tsDeadlineCh <- dl:
			case <-loopCtx.Done():
				cancel()
				return
			}
			opts = extractSpanReference(requests, opts[:0])
			err = c.processTSORequests(stream, requests, opts)
			close(done)
			requests = requests[:0]
		case <-loopCtx.Done():
			cancel()
			return
		}

		if err != nil {
			select {
			case <-loopCtx.Done():
				cancel()
				return
			default:
			}
			log.Error("[pd] getTS error", zap.Error(err))
			c.ScheduleCheckLeader()
			cancel()
			stream, cancel = nil, nil
		}
	}
}

func extractSpanReference(requests []*tsoRequest, opts []opentracing.StartSpanOption) []opentracing.StartSpanOption {
	for _, req := range requests {
		if span := opentracing.SpanFromContext(req.ctx); span != nil {
			opts = append(opts, opentracing.ChildOf(span.Context()))
		}
	}
	return opts
}

func (c *client) processTSORequests(stream pdpb.PD_TsoClient, requests []*tsoRequest, opts []opentracing.StartSpanOption) error {
	if len(opts) > 0 {
		span := opentracing.StartSpan("pdclient.processTSORequests", opts...)
		defer span.Finish()
	}

	start := time.Now()
	req := &pdpb.TsoRequest{
		Header: c.requestHeader(),
		Count:  uint32(len(requests)),
	}

	if err := stream.Send(req); err != nil {
		err = errors.WithStack(err)
		c.finishTSORequest(requests, 0, 0, err)
		return err
	}
	resp, err := stream.Recv()
	if err != nil {
		err = errors.WithStack(err)
		c.finishTSORequest(requests, 0, 0, err)
		return err
	}
	requestDuration.WithLabelValues("tso").Observe(time.Since(start).Seconds())
	if resp.GetCount() != uint32(len(requests)) {
		err = errors.WithStack(errTSOLength)
		c.finishTSORequest(requests, 0, 0, err)
		return err
	}

	physical, logical := resp.GetTimestamp().GetPhysical(), resp.GetTimestamp().GetLogical()
	// Server returns the highest ts.
	logical -= int64(resp.GetCount() - 1)
	c.finishTSORequest(requests, physical, logical, nil)
	return nil
}

func (c *client) finishTSORequest(requests []*tsoRequest, physical, firstLogical int64, err error) {
	for i := 0; i < len(requests); i++ {
		if span := opentracing.SpanFromContext(requests[i].ctx); span != nil {
			span.Finish()
		}
		requests[i].physical, requests[i].logical = physical, firstLogical+int64(i)
		requests[i].done <- err
	}
}

func (c *client) revokeTSORequest(err error) {
	n := len(c.tsoRequests)
	for i := 0; i < n; i++ {
		req := <-c.tsoRequests
		req.done <- err
	}
}

func (c *client) Close() {
	c.cancel()
	c.wg.Wait()

	c.revokeTSORequest(errors.WithStack(errClosing))

	c.connMu.Lock()
	defer c.connMu.Unlock()
	for _, cc := range c.connMu.clientConns {
		if err := cc.Close(); err != nil {
			log.Error("[pd] failed close grpc clientConn", zap.Error(err))
		}
	}
}

// leaderClient gets the client of current PD leader.
func (c *client) leaderClient() pdpb.PDClient {
	c.connMu.RLock()
	defer c.connMu.RUnlock()

	return pdpb.NewPDClient(c.connMu.clientConns[c.connMu.leader])
}

func (c *client) ScheduleCheckLeader() {
	select {
	case c.checkLeaderCh <- struct{}{}:
	default:
	}
}

func (c *client) GetClusterID(context.Context) uint64 {
	return c.clusterID
}

// For testing use.
func (c *client) GetLeaderAddr() string {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.connMu.leader
}

// For testing use. It should only be called when the client is closed.
func (c *client) GetURLs() []string {
	return c.urls
}

var tsoReqPool = sync.Pool{
	New: func() interface{} {
		return &tsoRequest{
			done: make(chan error, 1),
		}
	},
}

func (c *client) GetTSAsync(ctx context.Context) TSFuture {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span = opentracing.StartSpan("GetTSAsync", opentracing.ChildOf(span.Context()))
		ctx = opentracing.ContextWithSpan(ctx, span)
	}
	req := tsoReqPool.Get().(*tsoRequest)
	req.start = time.Now()
	req.ctx = ctx
	req.physical = 0
	req.logical = 0
	c.tsoRequests <- req

	return req
}

// TSFuture is a future which promises to return a TSO.
type TSFuture interface {
	// Wait gets the physical and logical time, it would block caller if data is not available yet.
	Wait() (int64, int64, error)
}

func (req *tsoRequest) Wait() (physical int64, logical int64, err error) {
	// If tso command duration is observed very high, the reason could be it
	// takes too long for Wait() be called.
	cmdDuration.WithLabelValues("tso_async_wait").Observe(time.Since(req.start).Seconds())
	select {
	case err = <-req.done:
		err = errors.WithStack(err)
		defer tsoReqPool.Put(req)
		if err != nil {
			cmdFailedDuration.WithLabelValues("tso").Observe(time.Since(req.start).Seconds())
			return 0, 0, err
		}
		physical, logical = req.physical, req.logical
		cmdDuration.WithLabelValues("tso").Observe(time.Since(req.start).Seconds())
		return
	case <-req.ctx.Done():
		return 0, 0, errors.WithStack(req.ctx.Err())
	}
}

func (c *client) GetTS(ctx context.Context) (physical int64, logical int64, err error) {
	resp := c.GetTSAsync(ctx)
	return resp.Wait()
}

func (c *client) GetRegion(ctx context.Context, key []byte) (*metapb.Region, *metapb.Peer, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span = opentracing.StartSpan("pdclient.GetRegion", opentracing.ChildOf(span.Context()))
		defer span.Finish()
	}
	start := time.Now()
	defer func() { cmdDuration.WithLabelValues("get_region").Observe(time.Since(start).Seconds()) }()

	ctx, cancel := context.WithTimeout(ctx, pdTimeout)
	resp, err := c.leaderClient().GetRegion(ctx, &pdpb.GetRegionRequest{
		Header:    c.requestHeader(),
		RegionKey: key,
	})
	cancel()

	if err != nil {
		cmdFailedDuration.WithLabelValues("get_region").Observe(time.Since(start).Seconds())
		c.ScheduleCheckLeader()
		return nil, nil, errors.WithStack(err)
	}
	return resp.GetRegion(), resp.GetLeader(), nil
}

func (c *client) GetPrevRegion(ctx context.Context, key []byte) (*metapb.Region, *metapb.Peer, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span = opentracing.StartSpan("pdclient.GetPrevRegion", opentracing.ChildOf(span.Context()))
		defer span.Finish()
	}
	start := time.Now()
	defer func() { cmdDuration.WithLabelValues("get_prev_region").Observe(time.Since(start).Seconds()) }()

	ctx, cancel := context.WithTimeout(ctx, pdTimeout)
	resp, err := c.leaderClient().GetPrevRegion(ctx, &pdpb.GetRegionRequest{
		Header:    c.requestHeader(),
		RegionKey: key,
	})
	cancel()

	if err != nil {
		cmdFailedDuration.WithLabelValues("get_prev_region").Observe(time.Since(start).Seconds())
		c.ScheduleCheckLeader()
		return nil, nil, errors.WithStack(err)
	}
	return resp.GetRegion(), resp.GetLeader(), nil
}

func (c *client) GetRegionByID(ctx context.Context, regionID uint64) (*metapb.Region, *metapb.Peer, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span = opentracing.StartSpan("pdclient.GetRegionByID", opentracing.ChildOf(span.Context()))
		defer span.Finish()
	}
	start := time.Now()
	defer func() { cmdDuration.WithLabelValues("get_region_byid").Observe(time.Since(start).Seconds()) }()

	ctx, cancel := context.WithTimeout(ctx, pdTimeout)
	resp, err := c.leaderClient().GetRegionByID(ctx, &pdpb.GetRegionByIDRequest{
		Header:   c.requestHeader(),
		RegionId: regionID,
	})
	cancel()

	if err != nil {
		cmdFailedDuration.WithLabelValues("get_region_byid").Observe(time.Since(start).Seconds())
		c.ScheduleCheckLeader()
		return nil, nil, errors.WithStack(err)
	}
	return resp.GetRegion(), resp.GetLeader(), nil
}

func (c *client) ScanRegions(ctx context.Context, key []byte, limit int) ([]*metapb.Region, []*metapb.Peer, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span = opentracing.StartSpan("pdclient.ScanRegions", opentracing.ChildOf(span.Context()))
		defer span.Finish()
	}
	start := time.Now()
	defer cmdDuration.WithLabelValues("scan_regions").Observe(time.Since(start).Seconds())
	ctx, cancel := context.WithTimeout(ctx, pdTimeout)
	resp, err := c.leaderClient().ScanRegions(ctx, &pdpb.ScanRegionsRequest{
		Header:   c.requestHeader(),
		StartKey: key,
		Limit:    int32(limit),
	})
	cancel()
	if err != nil {
		cmdFailedDuration.WithLabelValues("scan_regions").Observe(time.Since(start).Seconds())
		c.ScheduleCheckLeader()
		return nil, nil, errors.WithStack(err)
	}
	return resp.GetRegions(), resp.GetLeaders(), nil
}

func (c *client) GetStore(ctx context.Context, storeID uint64) (*metapb.Store, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span = opentracing.StartSpan("pdclient.GetStore", opentracing.ChildOf(span.Context()))
		defer span.Finish()
	}
	start := time.Now()
	defer func() { cmdDuration.WithLabelValues("get_store").Observe(time.Since(start).Seconds()) }()

	ctx, cancel := context.WithTimeout(ctx, pdTimeout)
	resp, err := c.leaderClient().GetStore(ctx, &pdpb.GetStoreRequest{
		Header:  c.requestHeader(),
		StoreId: storeID,
	})
	cancel()

	if err != nil {
		cmdFailedDuration.WithLabelValues("get_store").Observe(time.Since(start).Seconds())
		c.ScheduleCheckLeader()
		return nil, errors.WithStack(err)
	}
	store := resp.GetStore()
	if store == nil {
		return nil, errors.New("[pd] store field in rpc response not set")
	}
	if store.GetState() == metapb.StoreState_Tombstone {
		return nil, nil
	}
	return store, nil
}

func (c *client) GetAllStores(ctx context.Context, opts ...GetStoreOption) ([]*metapb.Store, error) {
	// Applies options
	options := &GetStoreOp{}
	for _, opt := range opts {
		opt(options)
	}

	if span := opentracing.SpanFromContext(ctx); span != nil {
		span = opentracing.StartSpan("pdclient.GetAllStores", opentracing.ChildOf(span.Context()))
		defer span.Finish()
	}
	start := time.Now()
	defer func() { cmdDuration.WithLabelValues("get_all_stores").Observe(time.Since(start).Seconds()) }()

	ctx, cancel := context.WithTimeout(ctx, pdTimeout)
	resp, err := c.leaderClient().GetAllStores(ctx, &pdpb.GetAllStoresRequest{
		Header:                 c.requestHeader(),
		ExcludeTombstoneStores: options.excludeTombstone,
	})
	cancel()

	if err != nil {
		cmdFailedDuration.WithLabelValues("get_all_stores").Observe(time.Since(start).Seconds())
		c.ScheduleCheckLeader()
		return nil, errors.WithStack(err)
	}
	stores := resp.GetStores()
	return stores, nil
}

func (c *client) UpdateGCSafePoint(ctx context.Context, safePoint uint64) (uint64, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span = opentracing.StartSpan("pdclient.UpdateGCSafePoint", opentracing.ChildOf(span.Context()))
		defer span.Finish()
	}
	start := time.Now()
	defer func() { cmdDuration.WithLabelValues("update_gc_safe_point").Observe(time.Since(start).Seconds()) }()

	ctx, cancel := context.WithTimeout(ctx, pdTimeout)
	resp, err := c.leaderClient().UpdateGCSafePoint(ctx, &pdpb.UpdateGCSafePointRequest{
		Header:    c.requestHeader(),
		SafePoint: safePoint,
	})
	cancel()

	if err != nil {
		cmdFailedDuration.WithLabelValues("update_gc_safe_point").Observe(time.Since(start).Seconds())
		c.ScheduleCheckLeader()
		return 0, errors.WithStack(err)
	}
	return resp.GetNewSafePoint(), nil
}

func (c *client) ScatterRegion(ctx context.Context, regionID uint64) error {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span = opentracing.StartSpan("pdclient.ScatterRegion", opentracing.ChildOf(span.Context()))
		defer span.Finish()
	}
	start := time.Now()
	defer func() { cmdDuration.WithLabelValues("scatter_region").Observe(time.Since(start).Seconds()) }()

	ctx, cancel := context.WithTimeout(ctx, pdTimeout)
	resp, err := c.leaderClient().ScatterRegion(ctx, &pdpb.ScatterRegionRequest{
		Header:   c.requestHeader(),
		RegionId: regionID,
	})
	cancel()
	if err != nil {
		return err
	}
	if resp.Header.GetError() != nil {
		return errors.Errorf("scatter region %d failed: %s", regionID, resp.Header.GetError().String())
	}
	return nil
}

func (c *client) GetOperator(ctx context.Context, regionID uint64) (*pdpb.GetOperatorResponse, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span = opentracing.StartSpan("pdclient.GetOperator", opentracing.ChildOf(span.Context()))
		defer span.Finish()
	}
	start := time.Now()
	defer func() { cmdDuration.WithLabelValues("get_operator").Observe(time.Since(start).Seconds()) }()

	ctx, cancel := context.WithTimeout(ctx, pdTimeout)
	defer cancel()
	return c.leaderClient().GetOperator(ctx, &pdpb.GetOperatorRequest{
		Header:   c.requestHeader(),
		RegionId: regionID,
	})
}

func (c *client) requestHeader() *pdpb.RequestHeader {
	return &pdpb.RequestHeader{
		ClusterId: c.clusterID,
	}
}

func addrsToUrls(addrs []string) []string {
	// Add default schema "http://" to addrs.
	urls := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if strings.Contains(addr, "://") {
			urls = append(urls, addr)
		} else {
			urls = append(urls, "http://"+addr)
		}
	}
	return urls
}
