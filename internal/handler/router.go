package handler

import (
	"net/http"

	"github.com/IgorBrizack/rinha-de-backend-2026/internal/scorer"
	"github.com/IgorBrizack/rinha-de-backend-2026/internal/usecase"
)

func NewRouter() http.Handler {
	uc := usecase.NewScoreFraud(&scorer.NoOp{})
	fraudHandler := NewFraudHandler(uc)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", Ready)
	mux.Handle("POST /fraud-score", fraudHandler)

	return mux
}
