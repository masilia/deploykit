package scaffold

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// registryImage matches an image written for deploykit's registry: ${REGISTRY}/name:${APP_VERSION}. The name is what the
// manifest builds and pushes.
var registryImage = regexp.MustCompile(`(?:\$\{REGISTRY[^}]*\}|\$REGISTRY)/([A-Za-z0-9][A-Za-z0-9._-]*)(?::|$)`)

// registryImageName returns the image name of a compose image reference, or "" when it does not use the registry.
func registryImageName(image string) string {
	m := registryImage.FindStringSubmatch(image)
	if m == nil {
		return ""
	}
	return m[1]
}

// dockerLines reads a Dockerfile as instructions: a line ending in a backslash is joined to the next one.
func dockerLines(dockerfile string) []string {
	f, err := os.Open(dockerfile) //nolint:gosec // a file of the project being scaffolded
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []string
	var cur strings.Builder
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			cur.WriteString(strings.TrimSuffix(line, "\\") + " ")
			continue
		}
		cur.WriteString(line)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// copySources lists the files and folders a Dockerfile copies from its build context: the sources of COPY and ADD, minus the
// flags, the destination, the stage copies (--from), URLs and anything built from a variable. A source that is only "."
// says nothing about where the files are, so it is left out: it means "the whole context, wherever that is".
func copySources(dockerfile string) []string {
	var out []string
	for _, line := range dockerLines(dockerfile) {
		fields := strings.Fields(line)
		if len(fields) < 3 || (!strings.EqualFold(fields[0], "COPY") && !strings.EqualFold(fields[0], "ADD")) {
			continue
		}
		var args []string
		fromStage := false
		for _, tok := range fields[1:] {
			switch {
			case strings.HasPrefix(tok, "--from="):
				fromStage = true
			case strings.HasPrefix(tok, "--"):
			default:
				args = append(args, tok)
			}
		}
		if fromStage || len(args) < 2 || strings.HasPrefix(args[0], "[") {
			continue
		}
		for _, src := range args[:len(args)-1] {
			if src == "." || src == "./" || strings.Contains(src, "$") || strings.Contains(src, "://") {
				continue
			}
			out = append(out, strings.TrimPrefix(src, "./"))
		}
	}
	return out
}

// contextHolds says whether every explicit source of a Dockerfile exists in the build context dir/ctx. known is false when the
// Dockerfile names no explicit source, so nothing can be checked.
func contextHolds(dir, ctx, dockerfile string) (holds, known bool) {
	sources := copySources(filepath.Join(dir, filepath.FromSlash(dockerfile)))
	if len(sources) == 0 {
		return false, false
	}
	for _, src := range sources {
		if matches, _ := filepath.Glob(filepath.Join(dir, filepath.FromSlash(ctx), filepath.FromSlash(src))); len(matches) == 0 {
			return false, true
		}
	}
	return true, true
}

// fitContext keeps the guessed build context unless the Dockerfile copies explicit paths that are not there but are in the other
// candidate (the Dockerfile's own folder, or the repository root).
func fitContext(dir string, ref ImageRef) ImageRef {
	other := "."
	if ref.Context == "." {
		other = path.Dir(ref.Dockerfile)
	}
	if other == "." && ref.Context == "." {
		return ref
	}
	if holds, known := contextHolds(dir, ref.Context, ref.Dockerfile); known && !holds {
		if alt, _ := contextHolds(dir, other, ref.Dockerfile); alt {
			ref.Context = other
		}
	}
	return ref
}

// leafStages returns the stages of a multi-stage Dockerfile that nothing else builds on: the ones meant to be an image
// (`docker build --target <stage>`). A stage another one starts FROM, or copies from, is only a step. It returns nothing
// for a single-stage Dockerfile, which needs no target.
func leafStages(dockerfile string) []string {
	var stages []string
	used := map[string]bool{}
	for _, line := range dockerLines(dockerfile) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			var image string
			for _, tok := range fields[1:] {
				if !strings.HasPrefix(tok, "--") {
					image = tok
					break
				}
			}
			used[image] = true
			for i, tok := range fields {
				if strings.EqualFold(tok, "AS") && i+1 < len(fields) {
					stages = append(stages, fields[i+1])
				}
			}
		case "COPY", "ADD":
			for _, tok := range fields[1:] {
				if v, ok := strings.CutPrefix(tok, "--from="); ok {
					used[v] = true
				}
			}
		}
	}
	if len(stages) < 2 {
		return nil
	}
	var leaves []string
	for _, st := range stages {
		if !used[st] {
			leaves = append(leaves, st)
		}
	}
	return leaves
}

// mapImages ties the images a compose file takes from the registry to what builds them: a Dockerfile in a folder named
// after the image (docker/varnish/Dockerfile for acme-varnish), a stage of a multi-stage Dockerfile named after it
// (--target php for acme-php), or a Dockerfile.<name>. The match is on the image name or on its last dash-separated word.
// An image nothing matches is reported in f.ImageNotes.
func mapImages(dir string, f *Findings) {
	var names []string
	for _, s := range f.Services {
		if n := registryImageName(s.Image); n != "" {
			names = append(names, n)
		}
	}
	for _, name := range dedupe(names) {
		keys := []string{name}
		if i := strings.LastIndex(name, "-"); i > 0 && i+1 < len(name) {
			keys = append(keys, name[i+1:])
		}
		if ref, ok := matchImage(dir, f.Dockerfiles, name, keys); ok {
			f.Images = append(f.Images, fitContext(dir, ref))
		} else {
			f.ImageNotes = append(f.ImageNotes, "no Dockerfile matches the image "+name+": add it to artifact.images by hand")
		}
	}
}

func matchImage(dir string, dockerfiles []string, name string, keys []string) (ImageRef, bool) {
	for _, key := range keys { // a Dockerfile in a folder of that name
		for _, df := range dockerfiles {
			if path.Base(path.Dir(df)) == key && path.Dir(df) != "." && path.Dir(df) != "docker" {
				return ImageRef{Name: name, Dockerfile: df, Context: path.Dir(df)}, true
			}
		}
	}
	for _, key := range keys { // a stage of a multi-stage Dockerfile
		for _, df := range dockerfiles {
			for _, stage := range leafStages(filepath.Join(dir, filepath.FromSlash(df))) {
				if stage == key {
					return ImageRef{Name: name, Dockerfile: df, Context: ".", Target: stage}, true
				}
			}
		}
	}
	for _, key := range keys { // Dockerfile.<name>
		for _, df := range dockerfiles {
			if strings.EqualFold(path.Base(df), "Dockerfile."+key) {
				return ImageRef{Name: name, Dockerfile: df, Context: "."}, true
			}
		}
	}
	return ImageRef{}, false
}

// envPlaceholders lists the variables of an env template that still say CHANGE_ME: what an operator or the install has to fill.
func envPlaceholders(path string) []string {
	f, err := os.Open(path) //nolint:gosec // a file of the project being scaffolded
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var keys []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(key), "#") {
			continue
		}
		if strings.Contains(value, "CHANGE_ME") {
			keys = append(keys, strings.TrimSpace(key))
		}
	}
	return dedupe(keys)
}
