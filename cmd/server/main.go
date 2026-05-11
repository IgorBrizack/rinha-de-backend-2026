package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"runtime"

	"github.com/IgorBrizack/rinha-de-backend-2026/internal/handler"
	"github.com/IgorBrizack/rinha-de-backend-2026/internal/scorer"
)

func main() {
	runtime.GOMAXPROCS(2)

	dataPath := getenv("DATASET_PATH", "/app/data/references.bin")
	mccPath := getenv("MCC_RISK_PATH", "/app/data/mcc_risk.json")

	knn, err := scorer.NewKNN(dataPath, mccPath)
	if err != nil {
		log.Fatalf("failed to load KNN dataset: %v", err)
	}
	log.Printf("dataset loaded: %d reference vectors", knn.Count())

	router := handler.NewRouter(knn)

	if socketPath := os.Getenv("SOCKET_PATH"); socketPath != "" {
		_ = os.Remove(socketPath)
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			log.Fatalf("failed to listen on unix socket %s: %v", socketPath, err)
		}
		defer ln.Close()
		if err := os.Chmod(socketPath, 0666); err != nil {
			log.Fatalf("failed to chmod socket: %v", err)
		}
		log.Printf("server listening on unix socket %s", socketPath)
		if err := http.Serve(ln, router); err != nil {
			log.Fatalf("server failed: %v", err)
		}
		return
	}

	port := getenv("PORT", "8080")
	log.Printf("server listening on :%s", port)
	if err := http.ListenAndServe(":"+port, router); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
