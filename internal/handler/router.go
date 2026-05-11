package handler

import (
	"net/http"

	"github.com/IgorBrizack/rinha-de-backend-2026/internal/domain"
	"github.com/IgorBrizack/rinha-de-backend-2026/internal/usecase"
)

func NewRouter(s domain.FraudScorer) http.Handler {
	uc := usecase.NewScoreFraud(s)
	fraudHandler := NewFraudHandler(uc)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", Ready)
	mux.Handle("POST /fraud-score", fraudHandler)

	return mux
}
