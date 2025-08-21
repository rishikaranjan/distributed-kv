package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"distributed-kv/kv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func tid() string { b := make([]byte, 8); _, _ = rand.Read(b); return hex.EncodeToString(b) }

func dial(addr string) kv.KVServiceClient {
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial %s: %v", addr, err)
	}
	return kv.NewKVServiceClient(conn)
}

func main() {
	key := flag.String("key", "", "key")
	val := flag.String("value", "", "value")
	regionsCSV := flag.String("regions", "127.0.0.1:50051,127.0.0.1:50052,127.0.0.1:50053", "comma-separated gRPC addresses")
	timeout := flag.Duration("timeout", 3*time.Second, "per-hop timeout")
	flag.Parse()
	if *key == "" || *val == "" {
		log.Fatal("need -key and -value")
	}

	addrs := strings.Split(*regionsCSV, ",")
	clients := make([]kv.KVServiceClient, 0, len(addrs))
	for _, a := range addrs {
		clients = append(clients, dial(strings.TrimSpace(a)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	tx := tid()
	stageKey := "__txn:" + tx + ":" + *key

	ok := 0
	for _, c := range clients {
		if _, err := c.Put(ctx, &kv.PutRequest{Key: stageKey, Value: *val}); err == nil {
			ok++
		}
	}
	if ok <= len(clients)/2 {
		log.Fatalf("prepare failed: %d/%d", ok, len(clients))
	}

	ok = 0
	for _, c := range clients {
		if _, err := c.Put(ctx, &kv.PutRequest{Key: *key, Value: *val}); err == nil {
			ok++
		}
	}
	if ok <= len(clients)/2 {
		log.Fatalf("commit failed: %d/%d", ok, len(clients))
	}

	fmt.Println("2PC ok tx=", tx)
}
