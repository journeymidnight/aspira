.PHONY: clean, profile
all: *.go
	swag init -g http_server.go
	go build
profile:
	go tool pprof http://127.0.0.1:8081/debug/pprof/profile
	go tool pprof http://127.0.0.1:8081/debug/pprof/heap
clean:
	rm -rf store1 store2 store3
	rm -rf db1 db2 db3
	mkdir store1 store2 store3
	rm -rf *.log
	killall -9 worker

