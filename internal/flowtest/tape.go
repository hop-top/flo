package flowtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"hop.top/kit/go/runtime/sideeffect"
	"hop.top/xrr"
	xexec "hop.top/xrr/adapters/exec"
	xhttp "hop.top/xrr/adapters/http"
)

// maxBody bounds what one cassette keeps of an HTTP body or of the files a
// process left in its step directory.
const maxBody = 8 << 20

// stepDirRe finds the scratch directory of a command node attempt in an argv
// (`{{ run.step_dir }}`): the directory is new for each attempt, so it is
// replaced by a placeholder when a call is matched against a cassette.
var stepDirRe = regexp.MustCompile(`[^\s=:"']*/flo-step-[0-9]+`)

const stepDirHolder = "<step_dir>"

// Tape records the exec and HTTP calls of one test, or replays them, through
// xrr cassettes in one directory. It wraps the two kit sideeffect seams every
// process and every declared HTTP node (and the eva check) goes through, so a
// replay never starts a process or opens a connection.
type Tape struct {
	mode Mode
	dir  string
	sess *xrr.FileSession

	mu       sync.Mutex
	calls    int
	replayed int
	misses   []string
}

// NewTape returns a Tape over dir. Recording creates it; replaying reads it,
// and a call without a cassette fails.
func NewTape(mode Mode, dir string) (*Tape, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("flowtest: mode %q is not replay or record", mode)
	}
	xm := xrr.ModeReplay
	if mode == ModeRecord {
		xm = xrr.ModeRecord
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("flowtest: cassette directory: %w", err)
		}
	}
	return &Tape{mode: mode, dir: dir, sess: xrr.NewSession(xm, xrr.NewFileCassette(dir))}, nil
}

// Stats returns how many calls went through the tape and how many of them a
// cassette answered.
func (t *Tape) Stats() (calls, replayed int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls, t.replayed
}

// Misses lists the calls a replay found no cassette for.
func (t *Tape) Misses() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.misses...)
}

func (t *Tape) count(replayed bool) {
	t.mu.Lock()
	t.calls++
	if replayed {
		t.replayed++
	}
	t.mu.Unlock()
}

func (t *Tape) miss(what string) error {
	t.mu.Lock()
	t.misses = append(t.misses, what)
	t.mu.Unlock()
	return fmt.Errorf("no recording for %s; record one with `flo flow test <flow> --record`", what)
}

// Exec wraps live, the seam that starts processes.
func (t *Tape) Exec(live sideeffect.Exec) sideeffect.Exec { return &execSeam{t: t, live: live} }

// HTTP wraps live, the seam that sends requests.
func (t *Tape) HTTP(live sideeffect.HTTP) sideeffect.HTTP { return &httpSeam{t: t, live: live} }

type execSeam struct {
	t    *Tape
	live sideeffect.Exec
}

var _ sideeffect.Exec = (*execSeam)(nil)

// execFile is a file a process left in its step directory.
type execFile struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
}

// execResponse is the exec cassette's response: what exec.Response holds, and
// the files the process wrote to its step directory (a command node may read
// its output from one).
type execResponse struct {
	xexec.Response `yaml:",inline"`
	Files          []execFile `yaml:"files,omitempty"`
}

func (*execResponse) AdapterID() string { return "exec" }

// Output implements sideeffect.Exec.
func (e *execSeam) Output(cmd *exec.Cmd) ([]byte, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	err := e.Run(cmd)
	return buf.Bytes(), err
}

// Run implements sideeffect.Exec.
func (e *execSeam) Run(cmd *exec.Cmd) error {
	stdin, err := takeStdin(cmd)
	if err != nil {
		return err
	}
	var stepDir string
	argv := make([]string, len(cmd.Args))
	for i, a := range cmd.Args {
		if stepDir == "" {
			stepDir = stepDirRe.FindString(a)
		}
		argv[i] = stepDirRe.ReplaceAllString(a, stepDirHolder)
	}
	req := &xexec.Request{Argv: argv, Stdin: string(stdin), Env: envKeys(cmd.Env)}

	var runErr error
	do := func() (xrr.Response, error) {
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = tee(cmd.Stdout, &so), tee(cmd.Stderr, &se)
		runErr = e.live.Run(cmd)
		resp := &execResponse{Response: xexec.Response{Stdout: so.String(), Stderr: se.String()}}
		var ee *exec.ExitError
		switch {
		case runErr == nil, errors.Is(runErr, exec.ErrWaitDelay):
		case errors.As(runErr, &ee):
			resp.ExitCode = ee.ExitCode()
		default:
			return nil, runErr // did not start: recorded as an error
		}
		files, ferr := readStepFiles(stepDir)
		if ferr != nil {
			return nil, ferr
		}
		resp.Files = files
		return resp, nil
	}
	out, err := e.t.sess.Record(context.Background(), &xexec.Adapter{}, req, do)
	if err != nil {
		if errors.Is(err, xrr.ErrCassetteMiss) {
			return e.t.miss("`" + strings.Join(argv, " ") + "`")
		}
		if e.t.mode == ModeReplay {
			e.t.count(true)
		}
		return err
	}
	if e.t.mode == ModeRecord {
		e.t.count(false)
		return runErr
	}
	raw, ok := out.(*xrr.RawResponse)
	if !ok {
		return fmt.Errorf("flowtest: replay of %T", out)
	}
	e.t.count(true)
	if err := writeOut(cmd.Stdout, str(raw.Payload["stdout"])); err != nil {
		return err
	}
	if err := writeOut(cmd.Stderr, str(raw.Payload["stderr"])); err != nil {
		return err
	}
	if err := restoreStepFiles(stepDir, raw.Payload["files"]); err != nil {
		return err
	}
	if code := num(raw.Payload["exit_code"]); code != 0 {
		return exitError(code)
	}
	return nil
}

// takeStdin reads the command's standard input so it can be matched, and puts
// it back for a live run.
func takeStdin(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdin == nil {
		return nil, nil
	}
	b, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		return nil, fmt.Errorf("flowtest: stdin: %w", err)
	}
	cmd.Stdin = bytes.NewReader(b)
	return b, nil
}

// envKeys keeps the names of what the command adds to the environment, never
// the values: cassettes carry no secrets.
func envKeys(env []string) map[string]string {
	base := map[string]bool{}
	for _, kv := range os.Environ() {
		base[kv] = true
	}
	out := map[string]string{}
	for _, kv := range env {
		if base[kv] {
			continue
		}
		if k, _, ok := strings.Cut(kv, "="); ok {
			out[k] = ""
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func tee(orig io.Writer, buf *bytes.Buffer) io.Writer {
	if orig == nil {
		return buf
	}
	return io.MultiWriter(orig, buf)
}

func writeOut(w io.Writer, s string) error {
	if w == nil || s == "" {
		return nil
	}
	_, err := io.WriteString(w, s)
	return err //nolint:wrapcheck // the caller's writer
}

func readStepFiles(dir string) ([]execFile, error) {
	if dir == "" {
		return nil, nil
	}
	var files []execFile
	total := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // inside the step directory walked
		if err != nil {
			return err
		}
		if total += len(b); total > maxBody {
			return fmt.Errorf("flowtest: the files of %s are over %d bytes: too large to record", dir, maxBody)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, execFile{Path: filepath.ToSlash(rel), Content: string(b)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("flowtest: step directory: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func restoreStepFiles(dir string, v any) error {
	list, _ := v.([]any)
	for _, it := range list {
		m, _ := it.(map[string]any)
		rel := str(m["path"])
		if dir == "" || !filepath.IsLocal(filepath.FromSlash(rel)) {
			return fmt.Errorf("flowtest: cassette file %q does not fit the step directory", rel)
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return fmt.Errorf("flowtest: %w", err)
		}
		if err := os.WriteFile(p, []byte(str(m["content"])), 0o600); err != nil {
			return fmt.Errorf("flowtest: %w", err)
		}
	}
	return nil
}

// exitError returns what a process that exited with code gives back from Run:
// an *exec.ExitError, which only a real exit can make.
func exitError(code int) error {
	if code < 0 || code > 255 {
		code = 1
	}
	err := exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(code)).Run() //nolint:gosec // a fixed script with an integer
	if err == nil {
		return fmt.Errorf("flowtest: exit %d", code)
	}
	return err //nolint:wrapcheck // the *exec.ExitError is the point
}

type httpSeam struct {
	t    *Tape
	live sideeffect.HTTP
}

var _ sideeffect.HTTP = (*httpSeam)(nil)

// kept are the headers a cassette keeps: those a reader of the recording
// needs, and none that can hold a credential.
var keptHeaders = []string{"Content-Type", "Accept", "Retry-After", "Link", "Etag", "Location"}

func keep(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range keptHeaders {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Do implements sideeffect.HTTP.
func (h *httpSeam) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("flowtest: request body: %w", err)
		}
		_ = req.Body.Close()
		body = b
		req.Body = io.NopCloser(bytes.NewReader(b))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}
	xreq := &xhttp.Request{Method: req.Method, URL: req.URL.String(), Headers: keep(req.Header), Body: string(body)}
	do := func() (xrr.Response, error) {
		resp, err := h.live.Do(req)
		if err != nil {
			return nil, err //nolint:wrapcheck // recorded as is
		}
		defer resp.Body.Close() //nolint:errcheck // read in full below
		rb, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return nil, fmt.Errorf("flowtest: response body: %w", err)
		}
		if len(rb) > maxBody {
			return nil, fmt.Errorf("flowtest: the response of %s %s is over %d bytes: too large to record", req.Method, req.URL, maxBody)
		}
		return &xhttp.Response{Status: resp.StatusCode, Headers: keep(resp.Header), Body: string(rb)}, nil
	}
	out, err := h.t.sess.Record(context.Background(), &xhttp.Adapter{}, xreq, do)
	if err != nil {
		if errors.Is(err, xrr.ErrCassetteMiss) {
			return nil, h.t.miss(req.Method + " " + req.URL.String())
		}
		if h.t.mode == ModeReplay {
			h.t.count(true)
		}
		return nil, err //nolint:wrapcheck // the recorded error
	}
	h.t.count(h.t.mode == ModeReplay)
	var status int
	var headers map[string]string
	var text string
	switch r := out.(type) {
	case *xhttp.Response:
		status, headers, text = r.Status, r.Headers, r.Body
	case *xrr.RawResponse:
		status, text = num(r.Payload["status"]), str(r.Payload["body"])
		headers = map[string]string{}
		if m, ok := r.Payload["headers"].(map[string]any); ok {
			for k, v := range m {
				headers[k] = str(v)
			}
		}
	default:
		return nil, fmt.Errorf("flowtest: http response of %T", out)
	}
	hdr := http.Header{}
	for k, v := range headers {
		hdr.Set(k, v)
	}
	return &http.Response{
		Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), StatusCode: status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: hdr,
		Body: io.NopCloser(strings.NewReader(text)), ContentLength: int64(len(text)), Request: req,
	}, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case uint64:
		return int(min(n, math.MaxInt32)) //nolint:gosec // a status or an exit code
	case float64:
		return int(n)
	}
	return 0
}
