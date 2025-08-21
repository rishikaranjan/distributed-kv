package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"distributed-kv/kv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "gRPC address of any region")
	getK := flag.String("get", "", "Key to GET")
	putK := flag.String("put", "", "Key to PUT")
	putV := flag.String("value", "", "Value to PUT (required with -put)")
	flag.Parse()

	conn, err := grpc.Dial(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := kv.NewKVServiceClient(conn)

	switch {
	case *putK != "":
		if *putV == "" {
			log.Fatal("-value required with -put")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		resp, err := client.Put(ctx, &kv.PutRequest{Key: *putK, Value: *putV})
		if err != nil {
			log.Fatalf("Put: %v", err)
		}
		fmt.Printf("OK=%v\n", resp.GetSuccess())

	case *getK != "":
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		resp, err := client.Get(ctx, &kv.GetRequest{Key: *getK, AllowStale: true})
		if err != nil {
			log.Fatalf("Get: %v", err)
		}
		fmt.Printf("value=%q ts=%d\n", resp.GetValue(), resp.GetTimestamp())

	default:
		fmt.Println("Usage:")
		fmt.Println("  kvcli -addr 127.0.0.1:50051 -put key -value val")
		fmt.Println("  kvcli -addr 127.0.0.1:50052 -get key")
	}
}
