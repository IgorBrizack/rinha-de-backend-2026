package main

import (
	"log"
	"net/http"
	"os"

	"github.com/IgorBrizack/rinha-de-backend-2026/internal/handler"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := ":" + port
	log.Printf("server listening on %s", addr)
	if err := http.ListenAndServe(addr, handler.NewRouter()); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
