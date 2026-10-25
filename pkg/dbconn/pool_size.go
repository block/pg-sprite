package dbconn

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// poolMaxConnsKey is the connection-string setting pgxpool reads the pool's
// size from.
const poolMaxConnsKey = "pool_max_conns"

// sizePool sets the pool's connection limit on pc, which pgxpool.ParseConfig
// built from cfg.URL: cfg.MaxConns when the caller set it; otherwise the
// URL's pool_max_conns, which pgxpool has already applied; otherwise
// pgxpool's default, raised to cfg.MaxConnsFloor when it is smaller.
func sizePool(pc *pgxpool.Config, cfg Config) error {
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
		return nil
	}
	if pc.MaxConns >= cfg.MaxConnsFloor {
		return nil
	}
	explicit, err := urlSetsMaxConns(cfg.URL)
	if err != nil {
		return err
	}
	if explicit {
		return nil
	}
	pc.MaxConns = cfg.MaxConnsFloor
	return nil
}

// urlSetsMaxConns reports whether the connection string names the pool's
// size. pgxpool.ParseConfig folds an absent pool_max_conns into its default
// and removes the setting, so its result cannot say which one it holds.
// pgconn.ParseConfig, the parser underneath it, keeps every setting it does
// not know among the runtime parameters, so parsing the same string with it
// answers the question from the same grammar, service file included.
func urlSetsMaxConns(url string) (bool, error) {
	cc, err := pgconn.ParseConfig(url)
	if err != nil {
		return false, fmt.Errorf("parse connection config for the pool size: %w", err)
	}
	_, ok := cc.RuntimeParams[poolMaxConnsKey]
	return ok, nil
}
