package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"

	"distributed-kv/kv"
	route "distributed-kv/pkg/route"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type backend struct {
	id     string
	addr   string
	conn   *grpc.ClientConn
	client kv.KVServiceClient
}

type router struct {
	kv.UnimplementedKVServiceServer

	ring       *route.HashRing
	backends   map[string]*backend
	rpcTimeout time.Duration

	// metrics
	reqs *prometheus.CounterVec
	lat  *prometheus.HistogramVec

	// chaos
	dropPut float64
	dropGet float64
	delayMS int
}

func (r *router) pick(key string) (*backend, error) {
	id, ok := r.ring.Get(key)
	if !ok {
		return nil, fmt.Errorf("no backends")
	}
	b, ok := r.backends[id]
	if !ok {
		return nil, fmt.Errorf("backend %q missing", id)
	}
	return b, nil
}

func (r *router) chaosMaybe(drop float64) bool {
	if r.delayMS > 0 {
		time.Sleep(time.Duration(r.delayMS) * time.Millisecond)
	}
	return drop > 0 && rand.Float64() < drop
}

func (r *router) Put(ctx context.Context, req *kv.PutRequest) (*kv.PutResponse, error) {
	start := time.Now()
	defer func() { r.lat.WithLabelValues("put").Observe(time.Since(start).Seconds()) }()
	if r.chaosMaybe(r.dropPut) {
		r.reqs.WithLabelValues("put", "chaos_drop").Inc()
		return &kv.PutResponse{Success: false}, nil
	}

	b, err := r.pick(req.Key)
	if err != nil {
		r.reqs.WithLabelValues("put", "no_backend").Inc()
		return &kv.PutResponse{Success: false}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, r.rpcTimeout)
	defer cancel()
	resp, err := b.client.Put(cctx, req)
	if err == nil && resp.GetSuccess() {
		r.reqs.WithLabelValues("put", "ok").Inc()
		return resp, nil
	}

	for id, ob := range r.backends {
		if id == b.id {
			continue
		}
		cctx2, cancel2 := context.WithTimeout(ctx, r.rpcTimeout)
		resp2, err2 := ob.client.Put(cctx2, req)
		cancel2()
		if err2 == nil && resp2.GetSuccess() {
			r.reqs.WithLabelValues("put", "ok_failover").Inc()
			return resp2, nil
		}
	}
	r.reqs.WithLabelValues("put", "fail").Inc()
	return &kv.PutResponse{Success: false}, nil
}

func (r *router) Get(ctx context.Context, req *kv.GetRequest) (*kv.GetResponse, error) {
	start := time.Now()
	defer func() { r.lat.WithLabelValues("get").Observe(time.Since(start).Seconds()) }()
	if r.chaosMaybe(r.dropGet) {
		r.reqs.WithLabelValues("get", "chaos_drop").Inc()
		return &kv.GetResponse{}, nil
	}

	b, err := r.pick(req.Key)
	if err != nil {
		r.reqs.WithLabelValues("get", "no_backend").Inc()
		return &kv.GetResponse{}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, r.rpcTimeout)
	defer cancel()
	resp, err := b.client.Get(cctx, req)
	if err == nil && resp.GetTimestamp() != 0 {
		r.reqs.WithLabelValues("get", "ok").Inc()
		return resp, nil
	}

	if req.GetAllowStale() {
		for id, ob := range r.backends {
			if id == b.id {
				continue
			}
			cctx2, cancel2 := context.WithTimeout(ctx, r.rpcTimeout)
			resp2, err2 := ob.client.Get(cctx2, req)
			cancel2()
			if err2 == nil && resp2.GetTimestamp() != 0 {
				r.reqs.WithLabelValues("get", "ok_failover").Inc()
				return resp2, nil
			}
		}
	}
	r.reqs.WithLabelValues("get", "miss").Inc()
	return &kv.GetResponse{}, nil
}

func parseBackends(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("empty backends")
	}
	for _, it := range strings.Split(s, ",") {
		kvp := strings.SplitN(strings.TrimSpace(it), "=", 2)
		if len(kvp) != 2 {
			return nil, fmt.Errorf("bad backend %q", it)
		}
		out[strings.TrimSpace(kvp[0])] = strings.TrimSpace(kvp[1])
	}
	return out, nil
}

func main() {
	listen := flag.String("listen", "127.0.0.1:6000", "router gRPC listen")
	backendsCSV := flag.String("backends",
		"region-0=127.0.0.1:50051,region-1=127.0.0.1:50052,region-2=127.0.0.1:50053",
		"comma-separated id=addr pairs")
	replicas := flag.Int("replicas", 128, "virtual nodes per backend")
	timeout := flag.Duration("rpc_timeout", 3*time.Second, "per-hop RPC timeout")

	metricsPort := flag.Int("metrics_port", 9100, "Prometheus metrics port")
	adminPort := flag.Int("admin_port", 7000, "admin HTTP port (/where, /cluster)")

	dropPut := flag.Float64("chaos_drop_put", 0, "probability to drop PUT [0..1]")
	dropGet := flag.Float64("chaos_drop_get", 0, "probability to drop GET [0..1]")
	delayMS := flag.Int("chaos_delay_ms", 0, "inject fixed delay in ms")

	flag.Parse()

	m, err := parseBackends(*backendsCSV)
	if err != nil {
		log.Fatalf("parse backends: %v", err)
	}

	r := &router{
		ring:       route.NewHashRing(*replicas),
		backends:   map[string]*backend{},
		rpcTimeout: *timeout,
		reqs: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "router_requests_total",
			Help: "Total requests through router",
		}, []string{"op", "status"}),
		lat: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "router_request_seconds",
			Help:    "Per-hop latency",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
		dropPut: *dropPut,
		dropGet: *dropGet,
		delayMS: *delayMS,
	}

	for id, addr := range m {
		conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Fatalf("dial %s (%s): %v", id, addr, err)
		}
		r.backends[id] = &backend{id: id, addr: addr, conn: conn, client: kv.NewKVServiceClient(conn)}
		r.ring.Add(id)
		log.Printf("router: added %s -> %s", id, addr)
	}

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		addr := fmt.Sprintf(":%d", *metricsPort)
		log.Printf("router metrics at %s/metrics", addr)
		_ = http.ListenAndServe(addr, mux)
	}()

	go func() {
		rt := r
		mux := http.NewServeMux()
		mux.HandleFunc("/where", func(w http.ResponseWriter, req *http.Request) {
			key := req.URL.Query().Get("key")
			if key == "" {
				http.Error(w, "missing key", http.StatusBadRequest)
				return
			}
			id, ok := rt.ring.Get(key)
			if !ok {
				http.Error(w, "no backends", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprintf(w, "%s\n", id)
		})
		mux.HandleFunc("/cluster", func(w http.ResponseWriter, req *http.Request) {
			for id, b := range rt.backends {
				fmt.Fprintf(w, "%s %s\n", id, b.addr)
			}
		})
		addr := fmt.Sprintf(":%d", *adminPort)
		log.Printf("router admin at %s (GET /where?key=..., /cluster)", addr)
		_ = http.ListenAndServe(addr, mux)
	}()

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen %s: %v", *listen, err)
	}
	s := grpc.NewServer()
	kv.RegisterKVServiceServer(s, r)

	log.Printf("router: listening at %s (backends=%d, replicas=%d)", *listen, len(m), *replicas)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
