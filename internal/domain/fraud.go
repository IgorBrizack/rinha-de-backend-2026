package domain

import "time"

type Transaction struct {
	Amount       float64
	Installments int
	RequestedAt  time.Time
}

type Customer struct {
	AvgAmount      float64
	TxCount24h     int
	KnownMerchants []string
}

type Merchant struct {
	ID        string
	MCC       string
	AvgAmount float64
}

type Terminal struct {
	IsOnline    bool
	CardPresent bool
	KmFromHome  float64
}

type LastTransaction struct {
	Timestamp     time.Time
	KmFromCurrent float64
}

type FraudInput struct {
	ID              string
	Transaction     Transaction
	Customer        Customer
	Merchant        Merchant
	Terminal        Terminal
	LastTransaction *LastTransaction
}

type FraudResult struct {
	Approved   bool
	FraudScore float64
}

type FraudScorer interface {
	Score(input FraudInput) FraudResult
}
