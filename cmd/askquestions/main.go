package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/pe-Gomes/askme-realtime/internal/api"
	"github.com/pe-Gomes/askme-realtime/internal/store/pgstore"
)

const env string = "PRODUCTION"

func main() {
	if env != "PRODUCTION" {
		if err := godotenv.Load(); err != nil {
			panic(fmt.Errorf("error loading env files: %w", err))
		}
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=prefer",
		os.Getenv("DATABASE_HOST"),
		os.Getenv("DATABASE_PORT"),
		os.Getenv("DATABASE_USER"),
		os.Getenv("DATABASE_PASSWORD"),
		os.Getenv("DATABASE_NAME"),
	))
	if err != nil {
		panic(fmt.Errorf("error creating pool to database: %w", err))
	}

	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		panic(fmt.Errorf("error connection to database: %w", err))
	}

	handler := api.NewHandler(pgstore.New(pool))

	go func() {
		if err := http.ListenAndServe(":8080", handler); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				panic(fmt.Errorf("error starting server: %w", err))
			}
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)

	<-quit
}
