package cfgfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"time"
)

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	err := enc.Encode(v)
	return b.Bytes(), err
}

func jsonDecoder(b []byte) *json.Decoder {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// Duration is a time.Duration written as a string such as "30s" or
// "1h30m".
//
// MarshalText has a value receiver so that values and fields both encode;
// UnmarshalText needs a pointer receiver. The same holds for Size.
type Duration time.Duration

//goland:noinspection GoMixedReceiverTypes
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

//goland:noinspection GoMixedReceiverTypes
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Size is a byte count written as a string: a non-negative integer with an
// optional unit B, KB, MB, GB, TB, PB (powers of 1000) or KiB, MiB, GiB,
// TiB, PiB (powers of 1024), such as "64MiB". It marshals with the largest
// binary unit that represents it exactly.
type Size uint64

var sizeUnits = []struct {
	name string
	mul  uint64
}{
	{"PiB", 1 << 50}, {"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"PB", 1e15}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
}

//goland:noinspection GoMixedReceiverTypes
func (s Size) MarshalText() ([]byte, error) {
	for _, u := range sizeUnits[:5] {
		if s != 0 && uint64(s)%u.mul == 0 {
			return []byte(strconv.FormatUint(uint64(s)/u.mul, 10) + u.name), nil
		}
	}
	return []byte(strconv.FormatUint(uint64(s), 10) + "B"), nil
}

//goland:noinspection GoMixedReceiverTypes
func (s *Size) UnmarshalText(b []byte) error {
	str := strings.TrimSpace(string(b))
	num := strings.TrimRight(str, "KMGTPiB ")
	unit := strings.TrimSpace(str[len(num):])
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return fmt.Errorf("cfgfile: bad size %q", str)
	}
	mul := uint64(1)
	if unit != "" {
		i := slices.IndexFunc(sizeUnits, func(u struct {
			name string
			mul  uint64
		}) bool {
			return u.name == unit
		})
		if i < 0 {
			return fmt.Errorf("cfgfile: bad size unit in %q", str)
		}
		mul = sizeUnits[i].mul
	}
	hi, lo := bits.Mul64(n, mul)
	if hi != 0 {
		return fmt.Errorf("cfgfile: size %q overflows", str)
	}
	*s = Size(lo)
	return nil
}
