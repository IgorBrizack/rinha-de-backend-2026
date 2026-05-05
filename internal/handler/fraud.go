package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/IgorBrizack/rinha-de-backend-2026/internal/domain"
	"github.com/IgorBrizack/rinha-de-backend-2026/internal/usecase"
)

type fraudRequest struct {
	ID          string          `json:"id"`
	Transaction txRequest       `json:"transaction"`
	Customer    customerRequest `json:"customer"`
	Merchant    merchantRequest `json:"merchant"`
	Terminal    terminalRequest `json:"terminal"`
	LastTx      *lastTxRequest  `json:"last_transaction"`
}

type txRequest struct {
	Amount       float64 `json:"amount"`
	Installments int     `json:"installments"`
	RequestedAt  string  `json:"requested_at"`
}

type customerRequest struct {
	AvgAmount      float64  `json:"avg_amount"`
	TxCount24h     int      `json:"tx_count_24h"`
	KnownMerchants []string `json:"known_merchants"`
}

type merchantRequest struct {
	ID        string  `json:"id"`
	MCC       string  `json:"mcc"`
	AvgAmount float64 `json:"avg_amount"`
}

type terminalRequest struct {
	IsOnline    bool    `json:"is_online"`
	CardPresent bool    `json:"card_present"`
	KmFromHome  float64 `json:"km_from_home"`
}

type lastTxRequest struct {
	Timestamp     string  `json:"timestamp"`
	KmFromCurrent float64 `json:"km_from_current"`
}

type fraudResponse struct {
	Approved   bool    `json:"approved"`
	FraudScore float64 `json:"fraud_score"`
}

type FraudHandler struct {
	uc *usecase.ScoreFraud
}

func NewFraudHandler(uc *usecase.ScoreFraud) *FraudHandler {
	return &FraudHandler{uc: uc}
}

func (h *FraudHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req fraudRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	input, err := mapToDomain(req)
	if err != nil {
		http.Error(w, "invalid timestamp format", http.StatusBadRequest)
		return
	}

	result := h.uc.Execute(input)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(fraudResponse{
		Approved:   result.Approved,
		FraudScore: result.FraudScore,
	})
}

func mapToDomain(req fraudRequest) (domain.FraudInput, error) {
	requestedAt, err := time.Parse(time.RFC3339, req.Transaction.RequestedAt)
	if err != nil {
		return domain.FraudInput{}, err
	}

	input := domain.FraudInput{
		ID: req.ID,
		Transaction: domain.Transaction{
			Amount:       req.Transaction.Amount,
			Installments: req.Transaction.Installments,
			RequestedAt:  requestedAt,
		},
		Customer: domain.Customer{
			AvgAmount:      req.Customer.AvgAmount,
			TxCount24h:     req.Customer.TxCount24h,
			KnownMerchants: req.Customer.KnownMerchants,
		},
		Merchant: domain.Merchant{
			ID:        req.Merchant.ID,
			MCC:       req.Merchant.MCC,
			AvgAmount: req.Merchant.AvgAmount,
		},
		Terminal: domain.Terminal{
			IsOnline:    req.Terminal.IsOnline,
			CardPresent: req.Terminal.CardPresent,
			KmFromHome:  req.Terminal.KmFromHome,
		},
	}

	if req.LastTx != nil {
		ts, err := time.Parse(time.RFC3339, req.LastTx.Timestamp)
		if err != nil {
			return domain.FraudInput{}, err
		}
		input.LastTransaction = &domain.LastTransaction{
			Timestamp:     ts,
			KmFromCurrent: req.LastTx.KmFromCurrent,
		}
	}

	return input, nil
}
