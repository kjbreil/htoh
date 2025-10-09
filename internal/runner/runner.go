package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	SourceDir   string
	WorkDir     string
	Interactive bool
	Keep        bool
	Silent      bool
	Workers     int
	Engine      string // cpu|qsv
	FFmpegPath  string
	SwapInplace bool
}

type job struct {
	Src       string
	Rel       string
	Probe     *ProbeInfo
	OutTarget string
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}
	state := NewState(cfg.WorkDir)
	if err := state.Load(); err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	ffprobePath := "ffprobe"
	if p := findSibling(cfg.FFmpegPath, "ffprobe"); p != "" {
		ffprobePath = p
	}

	if !cfg.Silent {
		fmt.Println("Indexing files (scanning and probing H.264)…")
	}
	files, err := listCandidates(ctx, ffprobePath, cfg.SourceDir, state)
	if err != nil {
		return err
	}
	if !cfg.Silent {
		fmt.Printf("Found %d candidate file(s).\n", len(files))
	}

	if cfg.Interactive {
		if len(files) == 0 {
			fmt.Println("Nothing to do.")
			return nil
		}
		fmt.Print("Proceed with all? [y/N]: ")
		var ans string
		_, _ = fmt.Scanln(&ans)
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans != "y" && ans != "yes" {
			return errors.New("aborted")
		}
	}

	// progress dashboard
	prog := NewProg()
	stopUI := make(chan struct{})
	if !cfg.Silent {
		go prog.RenderLoop(stopUI)
	}

	// Queue & workers
	jobs := make(chan job, cfg.Workers*2)
	var wg sync.WaitGroup
	errCh := make(chan error, cfg.Workers)

	for i := 0; i < cfg.Workers; i++ {
		wid := i + 1
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for jb := range jobs {
				prog.Update(workerID, func(r *Row) {
					r.WorkerID = workerID
					r.FileBase = filepath.Base(jb.Src)
					r.Phase = "transcoding"
					r.FPS = 0
					r.Speed = "-"
					r.OutTimeS = 0
					r.SizeBytes = 0
				})
				if err := state.Set(jb.Src, "processing"); err != nil && !cfg.Silent {
					fmt.Println("state write:", err)
				}
				if err := transcode(ctx, cfg, jb, workerID, prog); err != nil {
					_ = state.Set(jb.Src, "failed")
					prog.Fail(workerID, err.Error())
					errCh <- fmt.Errorf("%s: %w", jb.Src, err)
					continue
				}
				_ = state.Set(jb.Src, "done")
				prog.Done(workerID)
			}
		}(wid)
	}

	for _, pi := range files {
		rel, err := filepath.Rel(cfg.SourceDir, pi.path)
		if err != nil {
			rel = filepath.Base(pi.path)
		}
		out := filepath.Join(cfg.WorkDir, rel) + ".hevc.mkv"
		jobs <- job{Src: pi.path, Rel: rel, Probe: pi.info, OutTarget: out}
	}
	close(jobs)

	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()

	select {
	case <-waitDone:
	case <-ctx.Done():
	}
	close(stopUI)

	close(errCh)
	var nErr int
	for range errCh {
		nErr++
	}
	if nErr > 0 {
		return fmt.Errorf("completed with %d errors", nErr)
	}
	if !cfg.Silent {
		fmt.Println("All done.")
	}
	return nil
}

type candidate struct {
	path string
	info *ProbeInfo
}

func listCandidates(ctx context.Context, ffprobePath, root string, st *State) ([]candidate, error) {
	var out []candidate
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		switch ext {
		case ".mkv", ".mp4", ".mov", ".m4v":
		default:
			return nil
		}
		switch st.Get(p) {
		case "done", "processing":
			return nil
		}

		ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		pi, err := probe(ctx2, ffprobePath, p)
		if err != nil {
			_ = st.Set(p, "failed")
			return nil
		}
		if strings.EqualFold(pi.VideoCodec, "h264") || strings.EqualFold(pi.VideoCodec, "avc") {
			out = append(out, candidate{path: p, info: pi})
			_ = st.Set(p, "queued")
		}
		return nil
	})
	return out, err
}

func transcode(ctx context.Context, cfg Config, jb job, workerID int, prog *Prog) error {
	if err := os.MkdirAll(filepath.Dir(jb.OutTarget), 0o755); err != nil {
		return err
	}

	// build args with -progress pipe:1 and quiet errors
	var args []string
	switch strings.ToLower(cfg.Engine) {
	case "qsv":
		args = []string{
			"-hide_banner",
			"-v", "error",
			"-progress", "pipe:1",
			// input
			"-hwaccel", "qsv",
			"-c:v", "h264_qsv",
			"-i", jb.Src,
			// mapping
			"-map", "0",
			// video
			"-c:v", "hevc_qsv",
			"-preset", "veryfast",
			"-global_quality", "24",
			// audio/subs passthrough
			"-c:a", "copy",
			"-c:s", "copy",
			jb.OutTarget,
		}
	default:
		args = []string{
			"-hide_banner",
			"-v", "error",
			"-progress", "pipe:1",
			"-i", jb.Src,
			"-map", "0",
			"-c:v", "libx265",
			"-crf", "23",
			"-preset", "medium",
			"-c:a", "copy",
			"-c:s", "copy",
			jb.OutTarget,
		}
	}

	cmd := exec.CommandContext(ctx, cfg.FFmpegPath, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// keep stderr quiet; if needed, attach to os.Stderr
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return err
	}

	// Parse -progress lines
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			// key=value
			if i := strings.IndexByte(line, '='); i > 0 {
				key := line[:i]
				val := line[i+1:]
				switch key {
				case "out_time_ms":
					ms, _ := strconv.ParseFloat(val, 64)
					prog.Update(workerID, func(r *Row) { r.OutTimeS = ms / 1000000.0 })
				case "fps":
					fps, _ := strconv.ParseFloat(val, 64)
					prog.Update(workerID, func(r *Row) { r.FPS = fps })
				case "speed":
					prog.Update(workerID, func(r *Row) { r.Speed = val })
				case "total_size":
					sb, _ := strconv.ParseInt(val, 10, 64)
					prog.Update(workerID, func(r *Row) { r.SizeBytes = sb })
				case "progress":
					if val == "end" {
						// leave loop; ffmpeg will exit shortly
					}
				}
			}
		}
		done <- sc.Err()
	}()

	waitErr := cmd.Wait()
	readErr := <-done
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return waitErr
	}
	if cfg.SwapInplace {
		if err := SwapInPlaceCopy(jb.Src, jb.OutTarget); err != nil {
			return err
		}
	}
	return nil
}

func findSibling(ffmpeg, name string) string {
	if ffmpeg == "" {
		return ""
	}
	dir := filepath.Dir(ffmpeg)
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}
