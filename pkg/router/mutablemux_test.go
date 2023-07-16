/*
Copyright 2016 The Fission Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"context"
	"log"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/fission/fission/pkg/utils/httpserver"
	"github.com/fission/fission/pkg/utils/metrics"
)

func OldHandler(responseWriter http.ResponseWriter, request *http.Request) {
	_, err := responseWriter.Write([]byte("old handler"))
	if err != nil {
		log.Fatal(err)
	}
}
func NewHandler(responseWriter http.ResponseWriter, request *http.Request) {
	_, err := responseWriter.Write([]byte("new handler"))
	if err != nil {
		log.Fatal(err)
	}
}

func verifyRequest() {
	targetURL := "http://localhost:3333/aaa"
	testRequest(targetURL)
	targetURL2 := "http://localhost:3333/aaa/functionOutput"
	testRequestPost(targetURL2)
	
}

func spamServer(quit chan bool) {
	i := 0
	for {
		select {
		case <-quit:
			return
		default:
			i = i + 1
			resp, err := http.Get("http://localhost:3333/aaa")
			if err != nil {
				log.Panicf("failed to make get request %v: %v", i, err)
			}
			resp.Body.Close()
		}
	}
}
func subHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("sub Route"))
}

func subHandler2(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("sub2 Route"))
}
func TestMutableMux(t *testing.T) {
	// make a simple mutable router
	log.Print("Create mutable router")
	muxRouter := mux.NewRouter()
	muxRouter.Use(metrics.HTTPMetricMiddleware)
	subrouter:=muxRouter.PathPrefix("/aaa").Subrouter()
	subrouter.HandleFunc("/", OldHandler)
    subrouter.Handle("/functionOutput", http.HandlerFunc(subHandler)).Methods("POST")
  

	// subRouter := mux.NewRouter().PathPrefix("/aaa").Subrouter()
	// subRouter.Handle("/functionOutput", http.HandlerFunc(subHandler))
	config := zap.NewDevelopmentConfig()
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger, err := config.Build()
	panicIf(err)

	mr := newMutableRouter(logger, muxRouter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// start http server
	go httpserver.StartServer(ctx, logger, "router", "3333", mr)

	// continuously make requests, panic if any fails
	time.Sleep(100 * time.Millisecond)
	// q := make(chan bool)
	// go spamServer(q)

	time.Sleep(5 * time.Millisecond)

	// connect and verify old handler
	log.Print("Verify old handler")
	verifyRequest()

	// change the muxer
	log.Print("Change mux router")
	newMuxRouter := mux.NewRouter()
	newMuxRouter.Use(metrics.HTTPMetricMiddleware)
	subrouter=newMuxRouter.PathPrefix("/aaa").Subrouter()
	subrouter.Handle("/",http.HandlerFunc( NewHandler))
    subrouter.Handle("/functionOutput", http.HandlerFunc(subHandler2)).Methods("POST")
	// newMuxRouter.HandleFunc("/aaa", NewHandler)
	// subRouter = mux.NewRouter()
    // subRouter.Handle("/functionOutput", http.HandlerFunc(subHandler2))
    // muxRouter.PathPrefix("/aaa").Handler(subRouter)
	// subRouter = muxRouter.PathPrefix("/aaa").Subrouter()
	// subRouter.Handle("/functionOutput", http.HandlerFunc(subHandler2)).Methods("POST")
	mr.updateRouter(newMuxRouter)

	// connect and verify the new handler
	log.Print("Verify new handler")
	verifyRequest()

	// q <- true
	time.Sleep(100 * time.Millisecond)
}
