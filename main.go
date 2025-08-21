package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"distributed-kv/kv"
	"distributed-kv/raft"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// ========================= Config =========================
var (
	flagSnapshotEvery = flag.Uint64("snapshot_every", envUint64("SNAPSHOT_EVERY", 32), "Create a snapshot after this many new committed entries")
	flagWalRotate     = flag.Int64("wal_rotate_bytes", envInt64("WAL_ROTATE_BYTES", 8<<20), "Rotate WAL when current segment exceeds this many bytes")
	flagDataDir       = flag.String("data_dir", envStr("DATA_DIR", "data"), "Data directory for WAL and snapshots")
)

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func envUint64(k string, def uint64) uint64 {
	if v := os.Getenv(k); v != "" {
		var x uint64
		_, err := fmt.Sscanf(v, "%d", &x)
		if err == nil {
			return x
		}
	}
	return def
}
func envInt64(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		var x int64
		_, err := fmt.Sscanf(v, "%d", &x)
		if err == nil {
			return x
		}
	}
	return def
}

// ========================= TrueTime (toy) =========================
type TrueTime struct{ epsilon time.Duration }

func NewTrueTime() *TrueTime { return &TrueTime{epsilon: 6 * time.Millisecond} }

func (tt *TrueTime) Now() (earliest, latest time.Time) {
	now := time.Now()
	drift := time.Duration(rand.Int63n(int64(tt.epsilon)))
	return now.Add(-drift), now.Add(tt.epsilon - drift)
}

// ========================= Global Coordinator ======================
type Region struct {
	id      string
	node    *RaftNode
	address string
}

type GlobalCoordinator struct {
	regions []*Region
	tt      *TrueTime
	mutex   sync.Mutex
}

func NewGlobalCoordinator() *GlobalCoordinator {
	tt := NewTrueTime()
	coord := &GlobalCoordinator{tt: tt}

	peerAddrs := map[string]string{
		"region-0": "127.0.0.1:50051",
		"region-1": "127.0.0.1:50052",
		"region-2": "127.0.0.1:50053",
	}

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("region-%d", i)
		node := NewRaftNode(id, peerAddrs, *flagDataDir, *flagSnapshotEvery, *flagWalRotate) // includes WAL+snapshot recovery
		addr := peerAddrs[id]
		region := &Region{id: id, node: node, address: addr}
		coord.regions = append(coord.regions, region)

		go node.StartServer(addr)
		go node.StartMetricsServer(2112 + i)
		go node.Run() // election + heartbeat
	}

	return coord
}

// Send to any region; non-leader forwards to leader; leader replicates majority-ack.
func (gc *GlobalCoordinator) PutGlobal(key, value string) bool {
	gc.mutex.Lock()
	defer gc.mutex.Unlock()

	earliest, _ := gc.tt.Now()
	log.Printf("[GC] PutGlobal ts=%d key=%q val=%q", earliest.UnixNano(), key, value)

	r := gc.regions[rand.Intn(len(gc.regions))]
	conn, err := grpc.Dial(r.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("Dial error to %s: %v", r.address, err)
		return false
	}
	defer conn.Close()
	client := kv.NewKVServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	resp, err := client.Put(ctx, &kv.PutRequest{Key: key, Value: value})
	if err != nil {
		log.Printf("PutGlobal error: %v", err)
		return false
	}
	return resp.GetSuccess()
}

func (gc *GlobalCoordinator) GetGlobal(key string, allowStale bool, closestRegionID string) (string, bool) {
	var region *Region
	for _, r := range gc.regions {
		if r.id == closestRegionID {
			region = r
			break
		}
	}
	if region == nil {
		return "", false
	}
	conn, err := grpc.Dial(region.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("Dial error to %s: %v", region.address, err)
		return "", false
	}
	defer conn.Close()

	client := kv.NewKVServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := client.Get(ctx, &kv.GetRequest{Key: key, AllowStale: allowStale})
	if err != nil {
		log.Printf("Get error: %v", err)
		return "", false
	}
	return resp.GetValue(), resp.GetTimestamp() > 0
}

// ========================= WAL + Snapshot ==========================
const walRecordHeader = 4 // uint32 length prefix

type Snapshot struct {
	Index    uint64            `json:"index"`
	Term     uint64            `json:"term"`
	KV       map[string]string `json:"kv"`
	Created  int64             `json:"created"`
	LastTS   int64             `json:"last_ts"`
	LeaderID string            `json:"leader_id"`
}

func ensureDir(dir string) error { return os.MkdirAll(dir, 0o755) }

// WAL segments are:
//
//	<dir>/<id>.wal            (current)
//	<dir>/<id>.wal.<ts>       (rotated segments)
//
// Recovery reads all matching files sorted by name.
type WriteAheadLog struct {
	dir        string
	id         string
	curPath    string
	rotateSize int64

	mu sync.Mutex
	f  *os.File
}

func newWAL(dir, id string, rotateBytes int64) (*WriteAheadLog, error) {
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	cur := filepath.Join(dir, fmt.Sprintf("%s.wal", id))
	f, err := os.OpenFile(cur, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &WriteAheadLog{dir: dir, id: id, curPath: cur, rotateSize: rotateBytes, f: f}, nil
}

func (w *WriteAheadLog) Append(entry *raft.LogEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	b, err := proto.Marshal(entry)
	if err != nil {
		return err
	}
	var hdr [walRecordHeader]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.f.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.f.Write(b); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil { // fsync by default
		return err
	}

	// rotation check
	if w.rotateSize > 0 {
		if fi, err := w.f.Stat(); err == nil && fi.Size() >= w.rotateSize {
			if err := w.rotateLocked(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *WriteAheadLog) rotateLocked() error {
	// Close current, rename with timestamp, open new cur
	if err := w.f.Close(); err != nil {
		return err
	}
	ts := time.Now().UnixNano()
	newName := fmt.Sprintf("%s.wal.%d", w.id, ts)
	dst := filepath.Join(w.dir, newName)
	if err := os.Rename(w.curPath, dst); err != nil {
		return err
	}
	nf, err := os.OpenFile(w.curPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.f = nf
	return nil
}

func (w *WriteAheadLog) LoadAll() ([]*raft.LogEntry, error) {
	// Gather: rotated segments + current, sorted by filename
	files, err := os.ReadDir(w.dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	prefix := w.id + ".wal"
	for _, e := range files {
		name := e.Name()
		if name == prefix || strings.HasPrefix(name, prefix+".") {
			paths = append(paths, filepath.Join(w.dir, name))
		}
	}
	sort.Strings(paths)
	var entries []*raft.LogEntry
	for _, p := range paths {
		es, err := readWalFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		entries = append(entries, es...)
	}
	return entries, nil
}

func readWalFile(path string) ([]*raft.LogEntry, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	r := bufio.NewReader(file)
	var out []*raft.LogEntry
	for {
		hdr := make([]byte, walRecordHeader)
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		n := binary.BigEndian.Uint32(hdr)
		if n == 0 {
			continue
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		var e raft.LogEntry
		if err := proto.Unmarshal(buf, &e); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, nil
}

func (w *WriteAheadLog) ResetTruncateAndDeleteRotated() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Close current
	if err := w.f.Close(); err != nil {
		return err
	}
	// Remove rotated segments
	files, _ := os.ReadDir(w.dir)
	prefix := w.id + ".wal."
	for _, e := range files {
		if strings.HasPrefix(e.Name(), prefix) {
			_ = os.Remove(filepath.Join(w.dir, e.Name()))
		}
	}
	// Truncate current
	nf, err := os.Create(w.curPath)
	if err != nil {
		return err
	}
	w.f = nf
	return w.f.Sync()
}

// ---- Snapshot helpers ----
func snapshotPath(dir, id string) string { return filepath.Join(dir, fmt.Sprintf("%s.snap.json", id)) }

func loadSnapshot(dir, id string) (*Snapshot, error) {
	path := snapshotPath(dir, id)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Snapshot{KV: map[string]string{}}, nil
		}
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.KV == nil {
		s.KV = map[string]string{}
	}
	return &s, nil
}

func saveSnapshot(dir, id string, s *Snapshot) error {
	if err := ensureDir(dir); err != nil {
		return err
	}
	tmp := snapshotPath(dir, id) + ".tmp"
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, snapshotPath(dir, id))
}

// ========================= Raft Node ===============================
type Role int

const (
	Follower Role = iota
	Candidate
	Leader
)

type RaftNode struct {
	// gRPC servers
	raft.UnimplementedRaftServiceServer
	kv.UnimplementedKVServiceServer

	id          string
	peers       map[string]string // regionID -> addr
	dataDir     string
	snapEveryN  uint64
	rotateBytes int64

	role     Role
	term     uint64
	votedFor string

	log       []*raft.LogEntry // in-memory suffix after last snapshot
	commitIdx uint64
	votes     map[string]bool

	mutex       sync.Mutex
	lastHB      time.Time
	kvStore     map[string]string
	lastTs      int64
	leaderKnown string

	// persistence
	wal      *WriteAheadLog
	lastSnap uint64 // last snapshot index
	snapTerm uint64

	// metrics
	termGauge   prometheus.Gauge
	commitGauge prometheus.Gauge
	latencyHist prometheus.Histogram
}

func NewRaftNode(id string, peers map[string]string, dataDir string, snapshotEvery uint64, rotateBytes int64) *RaftNode {
	if err := ensureDir(dataDir); err != nil {
		log.Fatalf("[%s] ensure data dir: %v", id, err)
	}
	w, err := newWAL(dataDir, id, rotateBytes)
	if err != nil {
		log.Fatalf("[%s] wal open: %v", id, err)
	}

	n := &RaftNode{
		id:          id,
		peers:       peers,
		dataDir:     dataDir,
		snapEveryN:  snapshotEvery,
		rotateBytes: rotateBytes,
		role:        Follower,
		term:        0,
		votes:       make(map[string]bool),
		kvStore:     make(map[string]string),
		wal:         w,
		lastHB:      time.Now(),
		termGauge: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "raft_term", Help: "Current Raft term", ConstLabels: prometheus.Labels{"node": id},
		}),
		commitGauge: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "raft_commit_index", Help: "Raft commit index", ConstLabels: prometheus.Labels{"node": id},
		}),
		latencyHist: promauto.NewHistogram(prometheus.HistogramOpts{
			Name: "request_latency_seconds", Help: "Request latency", Buckets: prometheus.DefBuckets,
			ConstLabels: prometheus.Labels{"node": id},
		}),
	}

	// ---- Crash Recovery ----
	snap, err := loadSnapshot(dataDir, id)
	if err != nil {
		log.Fatalf("[%s] snapshot load: %v", id, err)
	}
	n.kvStore = snap.KV
	n.commitIdx = snap.Index
	n.lastSnap = snap.Index
	n.snapTerm = snap.Term
	n.lastTs = snap.LastTS
	n.leaderKnown = snap.LeaderID

	entries, err := n.wal.LoadAll()
	if err != nil {
		log.Fatalf("[%s] wal read: %v", id, err)
	}
	for _, e := range entries {
		if e.Index <= n.commitIdx {
			continue
		}
		n.applyEntry(e)
		n.commitIdx = e.Index
	}
	n.commitGauge.Set(float64(n.commitIdx))

	return n
}

// applyEntry assumes n.mutex is NOT held (we take it here).
func (n *RaftNode) applyEntry(e *raft.LogEntry) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	// keep a small in-memory suffix
	n.log = append(n.log, e)
	if len(n.log) > 256 {
		n.log = n.log[len(n.log)-256:]
	}
	n.kvStore[e.Key] = e.Value
	n.lastTs = e.Timestamp
}

// background loops
func (n *RaftNode) Run() {
	electionTicker := time.NewTicker(60 * time.Millisecond)
	heartbeatTicker := time.NewTicker(120 * time.Millisecond)
	defer electionTicker.Stop()
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-electionTicker.C:
			n.tickElection()
		case <-heartbeatTicker.C:
			n.tickHeartbeat()
		}
	}
}

func (n *RaftNode) StartServer(address string) {
	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatalf("[%s] listen error: %v", n.id, err)
	}
	s := grpc.NewServer()
	raft.RegisterRaftServiceServer(s, n)
	kv.RegisterKVServiceServer(s, n)
	log.Printf("[%s] gRPC listening at %s", n.id, address)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("[%s] serve error: %v", n.id, err)
	}
}

func (n *RaftNode) StartMetricsServer(port int) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	addr := fmt.Sprintf(":%d", port)
	go func() {
		log.Printf("[%s] metrics at %s/metrics", n.id, addr)
		if err := http.ListenAndServe(addr, mux); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[%s] metrics error: %v", n.id, err)
		}
	}()
}

// ================ Election + Heartbeat ================

func (n *RaftNode) tickElection() {
	n.mutex.Lock()
	role := n.role
	lastHB := n.lastHB
	n.mutex.Unlock()

	timeout := time.Duration(300+rand.Intn(300)) * time.Millisecond
	if role != Leader && time.Since(lastHB) > timeout {
		n.startElection()
	}
}

func (n *RaftNode) startElection() {
	n.mutex.Lock()
	n.role = Candidate
	n.term++
	term := n.term
	n.votedFor = n.id
	n.leaderKnown = ""
	n.lastHB = time.Now()
	n.mutex.Unlock()

	votes := 1 // self
	var mu sync.Mutex
	var wg sync.WaitGroup

	for peerID, addr := range n.peers {
		if peerID == n.id {
			continue
		}
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return
			}
			defer conn.Close()
			client := raft.NewRaftServiceClient(conn)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			resp, err := client.RequestVote(ctx, &raft.RequestVoteRequest{
				Term:        term,
				CandidateId: n.id,
			})
			if err == nil && resp.GetVoteGranted() {
				mu.Lock()
				votes++
				mu.Unlock()
			}
		}(addr)
	}
	wg.Wait()
	majority := (len(n.peers)/2 + 1)
	if votes >= majority {
		n.mutex.Lock()
		n.role = Leader
		n.leaderKnown = n.id
		n.lastHB = time.Now()
		n.termGauge.Set(float64(n.term))
		n.mutex.Unlock()
		log.Printf("[%s] became LEADER (term=%d, votes=%d)", n.id, term, votes)
	}
}

func (n *RaftNode) tickHeartbeat() {
	n.mutex.Lock()
	role := n.role
	term := n.term
	n.mutex.Unlock()
	if role != Leader {
		return
	}
	var wg sync.WaitGroup
	for peerID, addr := range n.peers {
		if peerID == n.id {
			continue
		}
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return
			}
			defer conn.Close()
			client := raft.NewRaftServiceClient(conn)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			_, _ = client.AppendEntries(ctx, &raft.AppendEntriesRequest{
				Term:     term,
				LeaderId: n.id,
				Entries:  nil, // heartbeat
			})
			cancel()
		}(addr)
	}
	wg.Wait()
}

// ================ RaftService (RPC handlers) =================

func (n *RaftNode) AppendEntries(ctx context.Context, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	start := time.Now()
	n.mutex.Lock()
	if req.Term >= n.term {
		if req.Term > n.term || n.role != Follower {
			n.term = req.Term
			n.role = Follower
			n.votedFor = ""
			n.termGauge.Set(float64(n.term))
		}
		n.leaderKnown = req.LeaderId
		n.lastHB = time.Now()
	}
	n.mutex.Unlock()

	// Apply entries: WAL -> apply -> advance commit
	for _, e := range req.Entries {
		if err := n.wal.Append(e); err != nil {
			log.Printf("[%s] wal append error: %v", n.id, err)
			return &raft.AppendEntriesResponse{Success: false}, nil
		}
		n.applyEntry(e)
		n.mutex.Lock()
		n.commitIdx = e.Index
		n.mutex.Unlock()
	}
	if len(req.Entries) > 0 {
		n.commitGauge.Set(float64(n.commitIdx))
		n.maybeSnapshot()
	}
	n.latencyHist.Observe(time.Since(start).Seconds())
	return &raft.AppendEntriesResponse{Success: true}, nil
}

func (n *RaftNode) RequestVote(ctx context.Context, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	if req.Term < n.term {
		return &raft.RequestVoteResponse{VoteGranted: false}, nil
	}
	if req.Term > n.term {
		n.term = req.Term
		n.role = Follower
		n.votedFor = ""
		n.termGauge.Set(float64(n.term))
	}
	// grant if not voted this term or same candidate
	if n.votedFor == "" || n.votedFor == req.CandidateId {
		n.votedFor = req.CandidateId
		n.lastHB = time.Now()
		return &raft.RequestVoteResponse{VoteGranted: true}, nil
	}
	return &raft.RequestVoteResponse{VoteGranted: false}, nil
}

// ================ KVService (client API) =====================
func (n *RaftNode) Put(ctx context.Context, req *kv.PutRequest) (*kv.PutResponse, error) {
	start := time.Now()

	n.mutex.Lock()
	role := n.role
	leader := n.leaderKnown
	term := n.term
	idx := n.commitIdx + 1 // after snapshot, commitIdx carries forward
	n.mutex.Unlock()

	// Forward if not leader
	if role != Leader && leader != "" && leader != n.id {
		addr := n.peers[leader]
		conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			n.latencyHist.Observe(time.Since(start).Seconds())
			return &kv.PutResponse{Success: false}, nil
		}
		defer conn.Close()
		client := kv.NewKVServiceClient(conn)
		ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		resp, err := client.Put(ctx2, req)
		n.latencyHist.Observe(time.Since(start).Seconds())
		if err != nil {
			return &kv.PutResponse{Success: false}, nil
		}
		return resp, nil
	}

	// Leader path: create entry, WAL, replicate, wait majority, commit -> apply
	ts := time.Now().UnixNano()
	entry := &raft.LogEntry{
		Term:      term,
		Index:     idx,
		Key:       req.Key,
		Value:     req.Value,
		Timestamp: ts,
	}
	// WAL first
	if err := n.wal.Append(entry); err != nil {
		n.latencyHist.Observe(time.Since(start).Seconds())
		return &kv.PutResponse{Success: false}, nil
	}

	// replicate
	var wg sync.WaitGroup
	acks := 1 // leader
	var mu sync.Mutex

	for peerID, addr := range n.peers {
		if peerID == n.id {
			continue
		}
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return
			}
			defer conn.Close()
			client := raft.NewRaftServiceClient(conn)
			ctx2, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			resp, err := client.AppendEntries(ctx2, &raft.AppendEntriesRequest{
				Term:     term,
				LeaderId: n.id,
				Entries:  []*raft.LogEntry{entry},
			})
			if err == nil && resp.GetSuccess() {
				mu.Lock()
				acks++
				mu.Unlock()
			}
		}(addr)
	}
	wg.Wait()

	majority := (len(n.peers)/2 + 1)
	if acks >= majority {
		// commit & apply locally
		n.applyEntry(entry)
		n.mutex.Lock()
		n.commitIdx = idx
		n.commitGauge.Set(float64(n.commitIdx))
		n.mutex.Unlock()
		n.maybeSnapshot()
		n.latencyHist.Observe(time.Since(start).Seconds())
		return &kv.PutResponse{Success: true}, nil
	}

	n.latencyHist.Observe(time.Since(start).Seconds())
	return &kv.PutResponse{Success: false}, nil
}

func (n *RaftNode) Get(ctx context.Context, req *kv.GetRequest) (*kv.GetResponse, error) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	val, ok := n.kvStore[req.Key]
	if !ok {
		return &kv.GetResponse{}, nil
	}
	return &kv.GetResponse{Value: val, Timestamp: n.lastTs}, nil
}

// ---- Snapshot/Compaction trigger ----
func (n *RaftNode) maybeSnapshot() {
	n.mutex.Lock()
	need := (n.commitIdx - n.lastSnap) >= n.snapEveryN
	idx := n.commitIdx
	term := n.term
	kvCopy := make(map[string]string, len(n.kvStore))
	for k, v := range n.kvStore {
		kvCopy[k] = v
	}
	lastTS := n.lastTs
	leader := n.leaderKnown
	dir := n.dataDir
	id := n.id
	n.mutex.Unlock()

	if !need {
		return
	}

	s := &Snapshot{
		Index:    idx,
		Term:     term,
		KV:       kvCopy,
		Created:  time.Now().UnixNano(),
		LastTS:   lastTS,
		LeaderID: leader,
	}
	if err := saveSnapshot(dir, id, s); err != nil {
		log.Printf("[%s] snapshot save error: %v", id, err)
		return
	}
	if err := n.wal.ResetTruncateAndDeleteRotated(); err != nil {
		log.Printf("[%s] wal truncate error: %v", id, err)
		return
	}
	n.mutex.Lock()
	n.lastSnap = idx
	// shrink in-memory suffix since everything up to idx is in snapshot
	var newSuffix []*raft.LogEntry
	for _, e := range n.log {
		if e.Index > idx {
			newSuffix = append(newSuffix, e)
		}
	}
	n.log = newSuffix
	n.mutex.Unlock()

	log.Printf("[%s] snapshotted at index=%d (term=%d), WAL rotated set deleted", id, idx, term)
}

// ========================= main ===============================
func main() {
	flag.Parse()
	rand.Seed(time.Now().UnixNano())

	gc := NewGlobalCoordinator()

	// Quick smoke test on boot
	ok := gc.PutGlobal("global_key", "global_value")
	log.Printf("PutGlobal success=%v", ok)
	if val, exists := gc.GetGlobal("global_key", true, "region-0"); exists {
		fmt.Printf("Read@region-0: %s\n", val)
	}

	// Keep servers running until Ctrl+C
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
