package main

import (
	"log"
	"net/http"
	"os"

	"github.com/snyk/snyk-code-review-exercise/api"
)

func main() {
	handler := api.New()

	// IE: use log for logging instead of fmt for extra features (i.e. timestamp)
	logger := log.New(os.Stdout, "DEPS API: ", log.Ldate|log.Ltime|log.Lshortfile)
	logger.Println("Server running on http://localhost:3000/")

	if err := http.ListenAndServe("localhost:3000", handler); err != nil {
		// IE: use log for logging instead of fmt for extra features (i.e. timestamp)
		logger.Fatal(err.Error())
		// or we can do:
		// panic(err.Error())

		// IE: now we don't need this anymore
		// os.Exit(1)
	}
}
