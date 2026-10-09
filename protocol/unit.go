package protocol

import (
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
)

// Sha256Pattern is how every input, output and blob is addressed: 64 lowercase hex digits.
var Sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// RunIdPattern is what a run id may be: safe in a URL path and an R2 key, and what the wire accepts.
var RunIdPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// MaximumUnitIdLength is the longest planned unit id, in bytes, the wire takes.
const MaximumUnitIdLength = 256

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CheckUnit checks what Decode can't: a unit a runner could only run wrongly is refused whole. The runner
// calls it before anything runs, and the coordinator before it hands a unit out.
func CheckUnit(unit Unit) error {
	if !RunIdPattern.MatchString(unit.Run) {
		return fmt.Errorf("run id %q: a letter or digit, then up to 127 letters, digits, dots, dashes and underscores", unit.Run)
	}
	if unit.Unit == "" || len(unit.Unit) > MaximumUnitIdLength {
		return fmt.Errorf("the unit needs a unit id of 1 to %d bytes", MaximumUnitIdLength)
	}
	switch {
	case len(unit.Argv) > 0 && unit.Test != nil:
		return fmt.Errorf("the unit carries both argv and a test job; it carries one")
	case unit.Test != nil:
		if err := CheckTestJob(*unit.Test); err != nil {
			return err
		}
	case len(unit.Argv) == 0 || unit.Argv[0] == "":
		return fmt.Errorf("the unit has no argv and no test job")
	}
	if unit.TimeoutSeconds <= 0 {
		return fmt.Errorf("the unit needs a positive timeoutSeconds")
	}
	if unit.BrokenExit < 0 || unit.BrokenExit > 255 {
		return fmt.Errorf("brokenExit %d isn't an exit code, 1 to 255", unit.BrokenExit)
	}
	if unit.SequenceStart < 0 {
		return fmt.Errorf("sequenceStart is a sequence, 0 or more")
	}
	if unit.Directory != "" && !filepath.IsLocal(filepath.FromSlash(unit.Directory)) {
		return fmt.Errorf("directory %q isn't inside the workspace", unit.Directory)
	}
	for name := range unit.Environment {
		if !environmentNamePattern.MatchString(name) {
			return fmt.Errorf("environment variable name %q isn't a plain name", name)
		}
	}
	for _, input := range unit.Inputs {
		if !filepath.IsLocal(filepath.FromSlash(input.Path)) {
			return fmt.Errorf("input path %q isn't inside the workspace", input.Path)
		}
		if !Sha256Pattern.MatchString(input.Sha256) {
			return fmt.Errorf("input %s: sha256 must be 64 lowercase hex digits", input.Path)
		}
		switch input.Archive {
		case "":
			if _, err := ParseMode(input.Mode); err != nil {
				return fmt.Errorf("input %s: %w", input.Path, err)
			}
		case "tar":
			if input.Mode != "" {
				return fmt.Errorf("input %s: a tar archive's entries carry their own modes, so mode doesn't apply", input.Path)
			}
		default:
			return fmt.Errorf("input %s: archive %q isn't tar", input.Path, input.Archive)
		}
	}
	for _, output := range unit.Outputs {
		if !fs.ValidPath(output.Glob) || output.Glob == "." {
			return fmt.Errorf("output glob %q isn't a path inside the workspace", output.Glob)
		}
		if _, err := filepath.Match(output.Glob, ""); err != nil {
			return fmt.Errorf("output glob %q: %w", output.Glob, err)
		}
	}
	if len(unit.Products) > 0 && unit.Test != nil {
		return fmt.Errorf("a test job takes no products yet: it builds its own command, with no place for them")
	}
	if len(unit.Products) > 0 && unit.ProductStore == "" {
		return fmt.Errorf("the unit has products but no productStore")
	}
	for _, product := range unit.Products {
		if !Sha256Pattern.MatchString(product.Key) {
			return fmt.Errorf("product key %q: 64 lowercase hex digits", product.Key)
		}
		if product.Directory == "" || !filepath.IsLocal(filepath.FromSlash(product.Directory)) {
			return fmt.Errorf("product %s: directory %q isn't inside the workspace", product.Key, product.Directory)
		}
	}
	if unit.ProductStore != "" {
		parsed, err := url.Parse(unit.ProductStore)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return fmt.Errorf("productStore %q isn't an http or https url", unit.ProductStore)
		}
	}
	if (len(unit.Inputs) > 0 || len(unit.Outputs) > 0) && (unit.Store == nil || unit.Store.Url == "") {
		return fmt.Errorf("the unit has inputs or outputs but no store")
	}
	for _, endpoint := range []*Endpoint{unit.Store, unit.Wire} {
		if endpoint == nil {
			continue
		}
		parsed, err := url.Parse(endpoint.Url)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return fmt.Errorf("endpoint %q isn't an http or https url", endpoint.Url)
		}
	}
	return nil
}

// ParseMode reads an input's octal mode. Empty means 0644; setuid, setgid and sticky bits are refused.
func ParseMode(text string) (fs.FileMode, error) {
	if text == "" {
		return 0o644, nil
	}
	value, err := strconv.ParseUint(text, 8, 32)
	if err != nil || value > 0o777 {
		return 0, fmt.Errorf("mode %q isn't an octal permission between 0 and 777", text)
	}
	return fs.FileMode(value), nil
}
