package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"distributed-kv/kv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func parseMap(s string) map[string]string {
	m := map[string]string{}
	s = strings.TrimSpace(s)
	if s == "" {
		return m
	}
	for _, p := range strings.Split(s, ",") {
		kvp := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kvp) == 2 {
			m[strings.TrimSpace(kvp[0])] = strings.TrimSpace(kvp[1])
		}
	}
	return m
}

func main() {
	port := flag.Int("port", 8080, "HTTP listen port")
	grpcAddr := flag.String("grpc", "127.0.0.1:6000", "gRPC address (router or region)")
	regionMapCSV := flag.String("region_map",
		"region-0=127.0.0.1:50051,region-1=127.0.0.1:50052,region-2=127.0.0.1:50053",
		"comma-separated region=addr map for direct reads")
	ttEpsMS := flag.Int("tt_epsilon_ms", 6, "TrueTime epsilon ms")
	flag.Parse()

	regionMap := parseMap(*regionMapCSV)

	dial, err := grpc.Dial(*grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer dial.Close()
	client := kv.NewKVServiceClient(dial)

	http.HandleFunc("/trutime", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		eps := time.Duration(*ttEpsMS) * time.Millisecond
		drift := time.Duration(rand.Int63n(int64(eps)))
		earliest := now.Add(-drift)
		latest := now.Add(eps - drift)
		fmt.Fprintf(w, `{"earliest":"%s","latest":"%s"}`+"\n", earliest.Format(time.RFC3339Nano), latest.Format(time.RFC3339Nano))
	})

	http.HandleFunc("/kv", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			key := r.URL.Query().Get("key")
			if key == "" {
				http.Error(w, "missing key", http.StatusBadRequest)
				return
			}
			allowStale := r.URL.Query().Get("allow_stale") == "1"
			region := r.URL.Query().Get("region")

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			if region != "" {
				if addr, ok := regionMap[region]; ok {
					d, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
					if err == nil {
						defer d.Close()
						rc := kv.NewKVServiceClient(d)
						resp, err := rc.Get(ctx, &kv.GetRequest{Key: key, AllowStale: allowStale})
						if err == nil && resp.GetTimestamp() != 0 {
							w.Header().Set("X-KV-Timestamp", fmt.Sprint(resp.GetTimestamp()))
							w.Header().Set("X-TT-Epsilon-MS", fmt.Sprintf("%d", *ttEpsMS))
							fmt.Fprintln(w, resp.GetValue())
							return
						}
					}
				}
			}

			resp, err := client.Get(ctx, &kv.GetRequest{Key: key, AllowStale: allowStale})
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			if resp.GetTimestamp() == 0 {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("X-KV-Timestamp", fmt.Sprint(resp.GetTimestamp()))
			w.Header().Set("X-TT-Epsilon-MS", fmt.Sprintf("%d", *ttEpsMS))
			fmt.Fprintln(w, resp.GetValue())

		case http.MethodPost:
			key := r.URL.Query().Get("key")
			val := r.URL.Query().Get("value")
			if key == "" || val == "" {
				http.Error(w, "missing key or value", http.StatusBadRequest)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			resp, err := client.Put(ctx, &kv.PutRequest{Key: key, Value: val})
			if err != nil || !resp.GetSuccess() {
				http.Error(w, "put failed", http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "use GET /kv?key=... or POST /kv?key=...&value=...", http.StatusMethodNotAllowed)
		}
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("httpgw listening on %s (proxying to gRPC %s)", addr, *grpcAddr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
