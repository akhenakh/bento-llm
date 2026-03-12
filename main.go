package main

import (
	"context"

	"github.com/warpstreamlabs/bento/public/service"

	// Import standard components
	_ "github.com/warpstreamlabs/bento/public/components/all"

	// Import your custom plugins
	_ "github.com/akhenakh/bento-llm/llm"
)

func main() {
	service.RunCLI(context.Background())
}
