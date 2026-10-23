// This negative control deliberately ignores crash=1. A crash experiment using
// it must fail verification instead of reporting successful fault isolation.
package main

import (
	"net"
	"net/http"
	"os"
)

func main() {
	err := http.ListenAndServe(net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT")), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	if err != nil {
		os.Exit(1)
	}
}
