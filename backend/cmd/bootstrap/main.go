// Command bootstrap creates a storage hierarchy in Homebox from an indented
// outline.
//
// Boxwright cannot cold-start itself: with no containers in Homebox every
// recommendation is trivially "make a new box". This turns an outline you can
// write in a minute into the locations to recommend against.
//
//	boxwright-bootstrap -f storage.txt          # plan only, writes nothing
//	boxwright-bootstrap -f storage.txt -apply   # create and annotate
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"boxwright/internal/bootstrap"
	"boxwright/internal/config"
	"boxwright/internal/homebox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bootstrap:", err)
		os.Exit(1)
	}
}

func run() error {
	file := flag.String("f", "storage.txt", "outline file (- for stdin)")
	apply := flag.Bool("apply", false, "actually create locations; without it nothing is written")
	capacity := flag.Int("capacity", 8, "capacityUnits for leaves with no capacity= setting")
	access := flag.String("access", "normal", "access for leaves with no access= setting")
	flag.Parse()

	in := os.Stdin
	if *file != "-" {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}

	switch *access {
	case "easy", "normal", "deep":
	default:
		return fmt.Errorf("-access=%q must be easy, normal or deep", *access)
	}

	roots, err := bootstrap.Parse(in)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.HomeboxAPIKey == "" {
		return fmt.Errorf("HOMEBOX_API_KEY is not set")
	}
	c := homebox.New(cfg.HomeboxBaseURL, cfg.HomeboxAPIKey)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	actions, err := bootstrap.Run(ctx, c, roots, bootstrap.Options{
		DefaultCapacity: *capacity,
		DefaultAccess:   *access,
		Apply:           *apply,
	})
	// Print whatever was planned or done even on failure, so a partial apply
	// is visible rather than silent.
	created, annotated := 0, 0
	for _, a := range actions {
		fmt.Println(" ", a)
		if a.Create {
			created++
		}
		if a.SetMeta {
			annotated++
		}
	}
	if err != nil {
		return err
	}

	fmt.Printf("\n%d locations, %d to create, %d to annotate\n", len(actions), created, annotated)
	if !*apply {
		fmt.Println("\nDry run: nothing was written. Re-run with -apply to make these changes.")
	}
	return nil
}
