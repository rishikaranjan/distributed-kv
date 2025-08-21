
ADDR  ?= 127.0.0.1:50051
ADDR2 ?= 127.0.0.1:50052
KEY   ?= hello
VALUE ?= world
SNAP  ?= 64
ROTATE?= 16777216
DATA  ?= data
BIN   ?= bin/dkv
LOG   ?= logs/dkv.log
PID   ?= dkv.pid

GW_PORT ?= 8081
GW_GRPC ?= 127.0.0.1:6000
GW_BIN  ?= bin/httpgw
GW_PID  ?= httpgw.$(GW_PORT).pid
GW_LOG  ?= logs/httpgw.$(GW_PORT).log

ROUTER_PORT     ?= 6000
ROUTER_REPLICAS ?= 128
ROUTER_BACKENDS ?= region-0=127.0.0.1:50051,region-1=127.0.0.1:50052,region-2=127.0.0.1:50053
ROUTER_BIN      ?= bin/router
ROUTER_PID      ?= router.$(ROUTER_PORT).pid
ROUTER_LOG      ?= logs/router.$(ROUTER_PORT).log
ROUTER_ARGS     ?=

.PHONY: run build start stop restart status logs force-stop put get metrics clean \
        build-gw start-gw stop-gw restart-gw status-gw logs-gw force-stop-gw \
        build-router start-router stop-router restart-router status-router logs-router force-stop-router \
        smoke bench router-metrics twopc

run:
	GOFLAGS= go run . -snapshot_every=$(SNAP) -wal_rotate_bytes=$(ROTATE) -data_dir=$(DATA)

build:
	mkdir -p bin
	go build -o $(BIN) .

start:
	@if [ -f $(PID) ] && ps -p $$(cat $(PID)) >/dev/null 2>&1; then \
		echo "dkv already running (pid $$(cat $(PID)))"; \
	else \
		$(MAKE) build; mkdir -p logs; \
		nohup $(BIN) -snapshot_every=$(SNAP) -wal_rotate_bytes=$(ROTATE) -data_dir=$(DATA) > $(LOG) 2>&1 & echo $$! > $(PID); \
		sleep 1; echo "Started dkv (pid $$(cat $(PID))). Logs: $(LOG)"; \
	fi

stop:
	-@kill $$(cat $(PID)) 2>/dev/null || true
	-@rm -f $(PID)
	@echo "Stopped dkv (if it was running)."

restart: stop start

force-stop:
	-@lsof -tiTCP:50051,50052,50053,2112,2113,2114 -sTCP:LISTEN | xargs kill -9 2>/dev/null || true
	-@rm -f $(PID)
	@echo "Force-stopped dkv and removed PID file."

status:
	@if [ -f $(PID) ] && ps -p $$(cat $(PID)) >/dev/null 2>&1; then \
		echo "dkv running (pid $$(cat $(PID)))"; \
	else \
		echo "dkv not running"; \
	fi

logs:
	@echo "Tailing $(LOG)... (Ctrl-C to stop)"
	@tail -f $(LOG)

put:
	go run ./cmd/kvcli -addr $(ADDR) -put $(KEY) -value $(VALUE)

get:
	go run ./cmd/kvcli -addr $(ADDR2) -get $(KEY)

metrics:
	@echo "region-0:" && curl -s http://127.0.0.1:2112/metrics | grep -E 'raft_term|raft_commit_index' || true
	@echo "region-1:" && curl -s http://127.0.0.1:2113/metrics | grep -E 'raft_term|raft_commit_index' || true
	@echo "region-2:" && curl -s http://127.0.0.1:2114/metrics | grep -E 'raft_term|raft_commit_index' || true

clean:
	rm -rf $(DATA) $(PID) $(LOG) bin

# -------- HTTP Gateway --------
build-gw:
	mkdir -p bin
	go build -o $(GW_BIN) ./cmd/httpgw

start-gw: build-gw
	@if [ -f $(GW_PID) ] && ps -p $$(cat $(GW_PID)) >/dev/null 2>&1; then \
		echo "httpgw already running (pid $$(cat $(GW_PID))) on port $(GW_PORT)"; \
	else \
		mkdir -p logs; \
		nohup $(GW_BIN) -port $(GW_PORT) -grpc $(GW_GRPC) > $(GW_LOG) 2>&1 & echo $$! > $(GW_PID); \
		sleep 1; echo "Started httpgw (pid $$(cat $(GW_PID))). Logs: $(GW_LOG) Port: $(GW_PORT)"; \
	fi

stop-gw:
	-@kill $$(cat $(GW_PID)) 2>/dev/null || true
	-@rm -f $(GW_PID)
	@echo "Stopped httpgw (if it was running)."

restart-gw: stop-gw start-gw

status-gw:
	@if [ -f $(GW_PID) ] && ps -p $$(cat $(GW_PID)) >/dev/null 2>&1; then \
		echo "httpgw running (pid $$(cat $(GW_PID))) on port $(GW_PORT)"; \
	else \
		echo "httpgw not running on port $(GW_PORT)"; \
	fi

logs-gw:
	@echo "Tailing $(GW_LOG)... (Ctrl-C to stop)"
	@tail -f $(GW_LOG)

force-stop-gw:
	-@lsof -tiTCP:$(GW_PORT) -sTCP:LISTEN | xargs kill -9 2>/dev/null || true
	-@rm -f $(GW_PID)
	@echo "Force-stopped gateway on port $(GW_PORT) and removed PID file."

# -------- Consistent-Hash Router --------
build-router:
	mkdir -p bin
	go build -o $(ROUTER_BIN) ./cmd/router

start-router: build-router
	@if [ -f $(ROUTER_PID) ] && ps -p $$(cat $(ROUTER_PID)) >/dev/null 2>&1; then \
		echo "router already running (pid $$(cat $(ROUTER_PID))) on port $(ROUTER_PORT)"; \
	else \
		mkdir -p logs; \
		nohup $(ROUTER_BIN) -listen 127.0.0.1:$(ROUTER_PORT) -replicas $(ROUTER_REPLICAS) -backends '$(ROUTER_BACKENDS)' $(ROUTER_ARGS) > $(ROUTER_LOG) 2>&1 & echo $$! > $(ROUTER_PID); \
		sleep 1; echo "Started router (pid $$(cat $(ROUTER_PID))). Logs: $(ROUTER_LOG) Port: $(ROUTER_PORT)"; \
	fi

stop-router:
	-@kill $$(cat $(ROUTER_PID)) 2>/dev/null || true
	-@rm -f $(ROUTER_PID)
	@echo "Stopped router (if it was running)."

restart-router: stop-router start-router

status-router:
	@if [ -f $(ROUTER_PID) ] && ps -p $$(cat $(ROUTER_PID)) >/dev/null 2>&1; then \
		echo "router running (pid $$(cat $(ROUTER_PID))) on port $(ROUTER_PORT)"; \
	else \
		echo "router not running on port $(ROUTER_PORT)"; \
	fi

logs-router:
	@echo "Tailing $(ROUTER_LOG)... (Ctrl-C to stop)"
	@tail -f $(ROUTER_LOG)

force-stop-router:
	-@lsof -tiTCP:$(ROUTER_PORT) -sTCP:LISTEN | xargs kill -9 2>/dev/null || true
	-@rm -f $(ROUTER_PID)
	@echo "Force-stopped router on port $(ROUTER_PORT) and removed PID file."

# -------- Helpers --------
smoke:
	$(MAKE) start-router ROUTER_PORT=$(ROUTER_PORT)
	$(MAKE) restart-gw GW_PORT=$(GW_PORT) GW_GRPC=127.0.0.1:$(ROUTER_PORT)
	@echo "✅ smoke passed"

bench:
	@echo "PUT x500..."
	@for i in $$(seq 1 500); do curl -s -XPOST 'http://127.0.0.1:$(GW_PORT)/kv?key=b'$$i'&value=v'$$i >/dev/null; done
	@echo "GET x500..."
	@for i in $$(seq 1 500); do curl -s 'http://127.0.0.1:$(GW_PORT)/kv?key=b'$$i >/dev/null; done
	@echo "Done."

router-metrics:
	curl -s http://127.0.0.1:9100/metrics | nl -ba | sed -n '120,160p'

twopc:
	go run ./cmd/coord -key $(KEY) -value $(VALUE) -regions 127.0.0.1:50051,127.0.0.1:50052,127.0.0.1:50053
