package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func healthz(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := map[string]string{"status": "200"}
	json.NewEncoder(w).Encode(response)
}

func main() {
	http.HandleFunc("/healthz", healthz)

	fmt.Println("Server is running on http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Printf("Error starting server: %s\n", err)
	}
}
