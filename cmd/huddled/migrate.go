package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/anthonyrego/huddle/internal/server/store"
)

func init() {
	register(&command{name: "migrate", summary: "apply the database migrations that haven't run (DATABASE_URL)", run: runMigrate})
}

func runMigrate(args []string) error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("set DATABASE_URL")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	defer st.Close()
	applied, err := st.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Println("up to date")
		return nil
	}
	fmt.Printf("applied %v\n", applied)
	return nil
}
