package main

import (
	"github.com/zerodenet/zboard-pay-plugins/internal/runtime"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
	"log"
)

func main() {
	server, err := runtime.Open()
	if err != nil {
		log.Fatal("initialize payment plugin: ", err)
	}
	defer server.Close()
	pluginv1.Serve(server)
}
