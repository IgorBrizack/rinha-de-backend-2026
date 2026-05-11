package main

import (
	"log"
	"net"
	"net/http"
	"os"

	"github.com/IgorBrizack/rinha-de-backend-2026/internal/handler"
)

func main() {
	router := handler.NewRouter()

	if socketPath := os.Getenv("SOCKET_PATH"); socketPath != "" {
		// Remove stale socket from a previous run (covers kill/crash/hot reload).
		_ = os.Remove(socketPath)
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			log.Fatalf("failed to listen on unix socket %s: %v", socketPath, err)
		}
		defer ln.Close()
		// Allow nginx (different user) to connect.
		if err := os.Chmod(socketPath, 0666); err != nil {
			log.Fatalf("failed to chmod socket: %v", err)
		}
		log.Printf("server listening on unix socket %s", socketPath)
		if err := http.Serve(ln, router); err != nil {
			log.Fatalf("server failed: %v", err)
		}
		return
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("server listening on :%s", port)
	if err := http.ListenAndServe(":"+port, router); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
