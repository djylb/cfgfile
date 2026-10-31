package app

import (
	"time"

	"github.com/djylb/cfgfile"
)

type Level string

func (l *Level) UnmarshalText(b []byte) error { *l = Level(b); return nil }

type Common struct {
	Name string `json:"name"`
	Rev  string `json:"rev"` // hidden by Config.Rev
}

type NoVersion struct {
	A string `json:"a"`
}

type Log struct {
	// Log level.
	Level  Level  `json:"level" enum:"debug,info" default:"info"`
	Format string `json:"format"`
}

type Config struct {
	Common
	Version int              `json:"version"`
	DataDir string           `json:"data_dir" doc:"data directory"`
	Log     Log              `json:"log"`
	Backup  *Log             `json:"backup,omitempty"`
	Timeout cfgfile.Duration `json:"timeout"`
	Poll    time.Duration    `json:"poll"`
	Limit   cfgfile.Size     `json:"limit"`
	Ports   []uint16         `json:"ports" default:"[80]"`
	Tags    map[string]bool  `json:"tags"`
	Skip    int              `json:"-"`
	Pair    [2]int           `json:"pair"`
	Names   []string         `json:"names"`
	Rev     int              `json:"rev"`
	hidden  int
}
