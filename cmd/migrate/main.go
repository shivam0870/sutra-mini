package main

import (
	"context"
	"log"
	"os"
	"time"

	"sutra/internal/db"
	"sutra/migrations"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := migrations.Apply(ctx, pool); err != nil {
		log.Fatal(err)
	}
	log.Print("database migrations applied")
}
