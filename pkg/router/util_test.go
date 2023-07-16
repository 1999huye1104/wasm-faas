package router

import (
	"bytes"
	"io"
	"log"
	"net/http"
)

func testRequest(targetURL string) {
	resp, err := http.Get(targetURL)
	if err != nil {
		log.Panicf("failed to make get request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Panicf("response status: %v", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Panic("failed to read response")
	}

	bodyStr := string(body)
	log.Printf("Server responded with %v", bodyStr)
	// if bodyStr != expectedResponse {
	// 	log.Panic("Unexpected response")
	// }
}

func testRequestPost(targetURL string) {
	requestBody := []byte("Hello, World!")

	// 创建请求
	resp, err := http.Post(targetURL, "application/octet-stream", bytes.NewBuffer(requestBody))
	if err != nil {
		log.Panicf("failed to make get request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Panicf("response status: %v", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Panic("failed to read response")
	}

	bodyStr := string(body)
	log.Printf("Server responded with %v", bodyStr)
	// if bodyStr != expectedResponse {
	// 	log.Panic("Unexpected response")
	// }
}
func panicIf(err error) {
	if err != nil {
		log.Panicf("Error: %v", err)
	}
}
