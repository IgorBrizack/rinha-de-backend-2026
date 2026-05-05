package handler

import "net/http"

func Ready(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
