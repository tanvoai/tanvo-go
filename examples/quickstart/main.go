// go run ./examples/quickstart: one free image, no API key needed.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tanvoai/tanvo-go"
)

func main() {
	ctx := context.Background()
	client := tanvo.New() // reads TANVO_API_KEY; without one it runs on the free tier

	img, err := client.GenerateImage(ctx, tanvo.ImageParams{
		Prompt: "a paper lantern floating over a misty lake at dawn",
		Aspect: "16:9",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(img.Status, img.Cost, "credits:", img.URL())

	path, err := client.Download(ctx, img, "out", 0)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("saved to", path)
}
