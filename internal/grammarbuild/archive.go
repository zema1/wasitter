package grammarbuild

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultMaxArchiveBytes bounds the compressed download. Official grammar
	// archives are much smaller; the bound protects CI and developer machines
	// from an unexpectedly large response or an accidental unbounded stream.
	DefaultMaxArchiveBytes int64 = 512 << 20
	// DefaultMaxExtractedBytes bounds the uncompressed source tree.
	DefaultMaxExtractedBytes int64 = 2 << 30
	// DefaultMaxExtractedFiles avoids pathological tarballs with millions of
	// tiny entries.
	DefaultMaxExtractedFiles = 100_000
)

// DownloadOptions controls source archive retrieval. URL is intended for
// tests or an explicitly configured mirror; normal builds derive a GitHub URL
// from the validated registry row.
type DownloadOptions struct {
	Client            *http.Client
	URL               string
	MaxArchiveBytes   int64
	MaxExtractedBytes int64
	MaxExtractedFiles int
	Attempts          int
	RetryDelay        time.Duration
}

// GrammarArchiveURL returns the canonical GitHub release archive URL for g.
func GrammarArchiveURL(g Grammar) string {
	// Validate is intentionally called here as this function is also useful to
	// callers constructing a row outside LoadRegistry. Return an empty string
	// for invalid data rather than emitting a URL that could be interpreted as
	// a different host/path by a caller.
	if err := g.Validate(); err != nil {
		return ""
	}
	return "https://github.com/" + g.Repository + "/archive/refs/tags/" + url.PathEscape(g.Tag) + ".tar.gz"
}

// DownloadAndExtract downloads g's archive, verifies its SHA-256 digest, and
// safely extracts it below destination. The returned path is the extracted
// top-level directory (the layout used by GitHub source archives).
func DownloadAndExtract(ctx context.Context, g Grammar, destination string, options DownloadOptions) (string, error) {
	if err := g.Validate(); err != nil {
		return "", fmt.Errorf("invalid grammar metadata: %w", err)
	}
	if destination == "" {
		return "", errors.New("destination must not be empty")
	}
	if err := rejectSymlinkComponents(destination); err != nil {
		return "", err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", fmt.Errorf("create extraction directory: %w", err)
	}
	if options.MaxArchiveBytes <= 0 {
		options.MaxArchiveBytes = DefaultMaxArchiveBytes
	}
	if options.MaxExtractedBytes <= 0 {
		options.MaxExtractedBytes = DefaultMaxExtractedBytes
	}
	if options.MaxExtractedFiles <= 0 {
		options.MaxExtractedFiles = DefaultMaxExtractedFiles
	}
	if options.Attempts <= 0 {
		options.Attempts = 3
	}
	if options.RetryDelay <= 0 {
		options.RetryDelay = 500 * time.Millisecond
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	archivePath, err := downloadArchive(ctx, g, destination, options, client)
	if err != nil {
		return "", err
	}
	defer os.Remove(archivePath)
	root, err := ExtractTarGz(archivePath, destination, options.MaxExtractedBytes, options.MaxExtractedFiles)
	if err != nil {
		return "", fmt.Errorf("extract grammar archive: %w", err)
	}
	return root, nil
}

func downloadArchive(ctx context.Context, g Grammar, destination string, options DownloadOptions, client *http.Client) (string, error) {
	targetURL := options.URL
	if targetURL == "" {
		targetURL = GrammarArchiveURL(g)
	}
	if targetURL == "" {
		return "", errors.New("cannot construct grammar archive URL")
	}
	u, err := url.Parse(targetURL)
	if err != nil || u == nil {
		return "", fmt.Errorf("invalid grammar archive URL %q", targetURL)
	}
	scheme := strings.ToLower(u.Scheme)
	if u.Host == "" || (scheme != "http" && scheme != "https") {
		return "", fmt.Errorf("invalid grammar archive URL %q", targetURL)
	}
	var lastErr error
	for attempt := 1; attempt <= options.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		path, err := os.CreateTemp(destination, ".grammar-archive-*")
		if err != nil {
			return "", fmt.Errorf("create archive temporary file: %w", err)
		}
		archivePath := path.Name()
		closeAndRemove := func() {
			_ = path.Close()
			_ = os.Remove(archivePath)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			closeAndRemove()
			return "", fmt.Errorf("create archive request: %w", err)
		}
		// GitHub occasionally serves a transient gateway response. A normal
		// user-agent also makes diagnostics on mirrors less surprising.
		req.Header.Set("User-Agent", "sitterwasm-grammar-builder/1")
		resp, err := client.Do(req)
		if err != nil {
			closeAndRemove()
			lastErr = fmt.Errorf("download grammar archive (attempt %d/%d): %w", attempt, options.Attempts, err)
			if attempt < options.Attempts {
				if waitRetry(ctx, options.RetryDelay, attempt) {
					continue
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return "", ctxErr
				}
			}
			return "", lastErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			closeAndRemove()
			lastErr = fmt.Errorf("download grammar archive (attempt %d/%d): HTTP %s: %s", attempt, options.Attempts, resp.Status, strings.TrimSpace(string(body)))
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				if attempt < options.Attempts {
					if waitRetry(ctx, options.RetryDelay, attempt) {
						continue
					}
					if ctxErr := ctx.Err(); ctxErr != nil {
						return "", ctxErr
					}
				}
			}
			return "", lastErr
		}

		hash := sha256.New()
		limited := io.LimitReader(resp.Body, options.MaxArchiveBytes+1)
		n, copyErr := io.Copy(path, io.TeeReader(limited, hash))
		bodyErr := resp.Body.Close()
		closeErr := path.Close()
		if copyErr != nil || bodyErr != nil || closeErr != nil {
			closeAndRemove()
			if copyErr == nil {
				copyErr = errors.Join(bodyErr, closeErr)
			}
			lastErr = fmt.Errorf("read grammar archive (attempt %d/%d): %w", attempt, options.Attempts, copyErr)
			if attempt < options.Attempts {
				if waitRetry(ctx, options.RetryDelay, attempt) {
					continue
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return "", ctxErr
				}
			}
			return "", lastErr
		}
		if n > options.MaxArchiveBytes {
			closeAndRemove()
			return "", fmt.Errorf("grammar archive exceeds %d bytes", options.MaxArchiveBytes)
		}
		actual := hex.EncodeToString(hash.Sum(nil))
		if !strings.EqualFold(actual, g.ArchiveSHA256) {
			closeAndRemove()
			return "", fmt.Errorf("grammar archive checksum mismatch: expected %s, got %s", strings.ToLower(g.ArchiveSHA256), actual)
		}
		return archivePath, nil
	}
	return "", lastErr
}

func waitRetry(ctx context.Context, base time.Duration, attempt int) bool {
	delay := base
	for i := 1; i < attempt; i++ {
		if delay >= 30*time.Second {
			delay = 30 * time.Second
			break
		}
		delay *= 2
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// ExtractTarGz safely extracts a gzip-compressed tar archive. Symlinks,
// hardlinks and device nodes are rejected because they can escape the output
// directory or make compiler input depend on host filesystem state. The
// returned path points at the archive's first (GitHub) top-level directory.
func ExtractTarGz(archivePath, destination string, maxBytes int64, maxFiles int) (string, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxExtractedBytes
	}
	if maxFiles <= 0 {
		maxFiles = DefaultMaxExtractedFiles
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("open gzip stream: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	if err := rejectSymlinkComponents(destination); err != nil {
		return "", err
	}
	if err := rejectSymlinkPath(destination, destination); err != nil {
		return "", err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", fmt.Errorf("create extraction destination: %w", err)
	}
	var top string
	var total int64
	var files int
	var entries int
	seen := make(map[string]struct{})
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar entry: %w", err)
		}
		// GitHub's archives include PAX metadata records (notably a
		// `pax_global_header`) before the real files. archive/tar exposes these
		// records to callers; they carry no filesystem object and must not be
		// mistaken for the archive's top-level directory.
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			continue
		}
		entries++
		if entries > maxFiles*2 {
			return "", fmt.Errorf("archive contains more than %d entries", maxFiles*2)
		}
		if hdr.Name == "" || strings.IndexByte(hdr.Name, 0) >= 0 || strings.Contains(hdr.Name, "\\") {
			return "", fmt.Errorf("unsafe tar entry name %q", hdr.Name)
		}
		// Inspect the archive spelling before normalizing it. Checking only the
		// cleaned path would turn entries such as `root/../file` into a
		// seemingly harmless `file` and silently change the archive layout.
		rawName := hdr.Name
		rawParts := strings.Split(rawName, "/")
		for i, part := range rawParts {
			// Tar directory headers conventionally carry one trailing slash.
			// It is structural syntax rather than an empty path component; all
			// other empty, dot, or parent components remain unsafe.
			if part == "" && i == len(rawParts)-1 && hdr.Typeflag == tar.TypeDir {
				continue
			}
			if part == ".." || part == "." || part == "" {
				return "", fmt.Errorf("unsafe tar entry name %q", hdr.Name)
			}
		}
		if len(rawName) > 1 && strings.HasSuffix(rawName, "/") {
			rawName = strings.TrimSuffix(rawName, "/")
		}
		cleanName := path.Clean(rawName)
		if cleanName == "." || strings.HasPrefix(cleanName, "/") {
			return "", fmt.Errorf("unsafe tar entry name %q", hdr.Name)
		}
		parts := strings.Split(cleanName, "/")
		for _, part := range parts {
			if part == ".." || part == "." || part == "" {
				return "", fmt.Errorf("unsafe tar entry name %q", hdr.Name)
			}
		}
		if top == "" {
			top = parts[0]
		} else if parts[0] != top {
			return "", fmt.Errorf("archive contains multiple top-level directories (%q and %q)", top, parts[0])
		}
		rel := strings.Join(parts[1:], "/")
		if rel == "" {
			if hdr.Typeflag != tar.TypeDir {
				return "", fmt.Errorf("top-level archive entry %q is not a directory", hdr.Name)
			}
			if hdr.Size != 0 {
				return "", fmt.Errorf("directory entry %q has non-zero size", hdr.Name)
			}
			target := filepath.Join(destination, filepath.FromSlash(cleanName))
			if err := rejectSymlinkPath(destination, target); err != nil {
				return "", err
			}
			if _, exists := seen[cleanName]; exists {
				return "", fmt.Errorf("duplicate tar entry %q", hdr.Name)
			}
			seen[cleanName] = struct{}{}
			if err := os.MkdirAll(target, 0o700); err != nil {
				return "", fmt.Errorf("create top-level directory %q: %w", hdr.Name, err)
			}
			continue
		}
		// Validate the final host path using filepath.Rel as an additional
		// defense against platform-specific separator behavior.
		target := filepath.Join(destination, filepath.FromSlash(cleanName))
		relTarget, err := filepath.Rel(destination, target)
		if err != nil || relTarget == ".." || strings.HasPrefix(relTarget, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("tar entry escapes destination: %q", hdr.Name)
		}
		if err := rejectSymlinkPath(destination, target); err != nil {
			return "", err
		}
		if _, exists := seen[cleanName]; exists {
			return "", fmt.Errorf("duplicate tar entry %q", hdr.Name)
		}
		seen[cleanName] = struct{}{}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if hdr.Size != 0 {
				return "", fmt.Errorf("directory entry %q has non-zero size", hdr.Name)
			}
			if err := os.MkdirAll(target, 0o700); err != nil {
				return "", fmt.Errorf("create directory %q: %w", hdr.Name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || hdr.Size > maxBytes-total {
				return "", fmt.Errorf("extracted grammar exceeds %d bytes", maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return "", fmt.Errorf("create parent for %q: %w", hdr.Name, err)
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return "", fmt.Errorf("create extracted file %q: %w", hdr.Name, err)
			}
			written, copyErr := io.CopyN(out, tr, hdr.Size)
			closeErr := out.Close()
			if copyErr != nil || closeErr != nil || written != hdr.Size {
				_ = os.Remove(target)
				if copyErr == nil {
					copyErr = closeErr
				}
				if copyErr == nil {
					copyErr = io.ErrUnexpectedEOF
				}
				return "", fmt.Errorf("extract file %q: %w", hdr.Name, copyErr)
			}
			if mode := os.FileMode(hdr.Mode) & 0o777; mode != 0 {
				_ = os.Chmod(target, mode)
			}
			total += hdr.Size
			files++
			if files > maxFiles {
				return "", fmt.Errorf("archive contains more than %d files", maxFiles)
			}
		case tar.TypeSymlink, tar.TypeLink:
			return "", fmt.Errorf("archive entry %q uses unsupported link type", hdr.Name)
		default:
			return "", fmt.Errorf("archive entry %q uses unsupported type %d", hdr.Name, hdr.Typeflag)
		}
	}
	if top == "" {
		return "", errors.New("archive contains no entries")
	}
	root := filepath.Join(destination, filepath.FromSlash(top))
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("top-level entry is not a directory")
		}
		return "", fmt.Errorf("invalid archive root %q: %w", top, err)
	}
	return root, nil
}

// rejectSymlinkPath prevents extraction from following a symlink that was
// already present in the destination (or created by an earlier archive entry).
// The archive itself cannot create symlinks because those entries are rejected
// above. Checking every existing component keeps a caller-provided destination
// from redirecting writes outside the temporary extraction tree.
func rejectSymlinkPath(base, target string) error {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("extraction path %s is outside destination", target)
	}
	current := base
	if rel == "." {
		if info, statErr := os.Lstat(current); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("extraction destination %s is a symlink", current)
		}
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			// Missing components will be created below; no existing symlink can
			// redirect them at this point.
			continue
		}
		if statErr != nil {
			return fmt.Errorf("inspect extraction path %s: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("extraction path %s is a symlink", current)
		}
	}
	return nil
}

// rejectSymlinkComponents checks every existing component of an absolute or
// relative path. filepath.MkdirAll follows symlinks in parent directories, so
// checking only the final destination (or only the path below it) would let a
// caller redirect extraction through a pre-existing parent link.
func rejectSymlinkComponents(target string) error {
	if target == "" {
		return errors.New("path must not be empty")
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve path %s: %w", target, err)
	}
	abs = filepath.Clean(abs)
	volume := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, volume)
	current := volume
	if filepath.IsAbs(abs) {
		if current == "" {
			current = string(filepath.Separator)
		} else if !strings.HasSuffix(current, string(filepath.Separator)) {
			current += string(filepath.Separator)
		}
	}
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return fmt.Errorf("inspect path %s: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %s is a symlink", current)
		}
	}
	return nil
}
