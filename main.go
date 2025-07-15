package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rwcarlsen/goexif/exif"
	"github.com/schollz/progressbar/v3"
)

var (
	photoExts = map[string]bool{
		".jpg": true, ".jpeg": true,
		".mp4": true, ".mov": true,
		".avi": true, ".3gp": true,
	}

	excludeExts = map[string]bool{
		".png": true, ".webp": true, ".gif": true,
	}

	manifestMu sync.Mutex
)

type transferSample struct {
	time  time.Time
	bytes int64
}

var alreadyCopied = 0

func main() {
	var manifestPathFlag string
	flag.StringVar(&manifestPathFlag, "manifest", "", "Path to manifest file (default $HOME/.copy_sort_manifest.txt)")
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		log.Fatalf("Usage: %s [-manifest /path/to/manifest.txt] <source_dir> <dest_dir>", os.Args[0])
	}
	srcRoot := args[0]
	destRoot := args[1]

	manifestPath := manifestPathFlag
	if manifestPath == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("Failed to get user home dir: %v", err)
		}
		manifestPath = filepath.Join(homeDir, ".copy_sort_manifest.txt")
	}
	fmt.Printf("Using manifest %s\n", manifestPath)

	copiedSet := loadManifest(manifestPath)

	var jobs []string
	_ = filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(srcRoot, path)
		if strings.Contains(path, "RECYCLE.BIN") {
			return nil
		}
		if _, copied := copiedSet[rel]; !copied {
			jobs = append(jobs, path)
		} else {
			alreadyCopied += 1
		}
		// fmt.Printf("Skipping %s\n", rel)
		return nil
	})

	totalJobs := len(jobs)
	if totalJobs == 0 {
		fmt.Println("No files to copy. You're done!")
		return
	}

	var processedCount int64
	var totalBytes int64
	var samples []transferSample
	var samplesMu sync.Mutex
	startTime := time.Now()

	bar := progressbar.NewOptions(totalJobs+alreadyCopied,
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionSetWidth(40),
		progressbar.OptionShowCount(),
		progressbar.OptionThrottle(100*time.Millisecond),
		progressbar.OptionClearOnFinish(),
		progressbar.OptionSetDescription("[cyan]Copying...[reset]"),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "[green]=[reset]",
			SaucerHead:    "[green]>[reset]",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)
	bar.Add(alreadyCopied)

	manifestFile, err := os.OpenFile(manifestPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Failed to open manifest: %v", err)
	}
	defer manifestFile.Close()
	manifestWriter := bufio.NewWriter(manifestFile)
	defer manifestWriter.Flush()

	/*
		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				count := atomic.LoadInt64(&processedCount)
				samplesMu.Lock()
				if len(samples) >= 2 {
					first := samples[0]
					last := samples[len(samples)-1]
					deltaBytes := last.bytes - first.bytes
					deltaTime := last.time.Sub(first.time).Seconds()
					if deltaTime > 0 {
						speed := float64(deltaBytes) / deltaTime
						remaining := totalJobs - int(count)
						eta := time.Duration(float64(remaining) * (float64(deltaTime) / float64(len(samples)-1)))
						fmt.Printf("\tCopying (%s @ %s/s, ETA %s)\n", humanSize(atomic.LoadInt64(&totalBytes)), humanSize(int64(speed)), humanDuration(eta))
					}
				}
				samplesMu.Unlock()
			}
		}()
	*/

	const workerCount = 8
	var wg sync.WaitGroup
	chunkSize := (len(jobs) + workerCount - 1) / workerCount

	for i := 0; i < workerCount; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(jobs) {
			end = len(jobs)
		}
		chunk := jobs[start:end]
		if len(chunk) == 0 {
			continue
		}
		wg.Add(1)
		go func(fileGroup []string) {
			defer wg.Done()
			for _, path := range fileGroup {
				info, err := os.Stat(path)
				if err != nil {
					continue
				}
				relPath, err := filepath.Rel(srcRoot, path)
				if err != nil {
					continue
				}

				ext := strings.ToLower(filepath.Ext(path))
				if excludeExts[ext] {
					continue
				}

				var destPath string
				if photoExts[ext] {
					timestamp, err := extractTimestamp(path)
					if err != nil {
						timestamp = info.ModTime()
					}
					destPath = filepath.Join(destRoot, "sorted_photos",
						fmt.Sprintf("%04d", timestamp.Year()),
						fmt.Sprintf("%02d", int(timestamp.Month())),
						fmt.Sprintf("%02d", timestamp.Day()),
						filepath.Base(path),
					)
				} else {
					destPath = filepath.Join(destRoot, relPath)
				}

				copiedBytes, err := copyFile(path, destPath, info.ModTime())
				if err == nil {
					now := time.Now()
					total := atomic.AddInt64(&totalBytes, copiedBytes)
					atomic.AddInt64(&processedCount, 1)

					samplesMu.Lock()
					samples = append(samples, transferSample{time: now, bytes: total})
					if len(samples) > 20 {
						samples = samples[1:]
					}
					samplesMu.Unlock()

					bar.Add(1)
					syncManifestWrite(manifestWriter, relPath)
				}
			}
		}(chunk)
	}

	wg.Wait()
	bar.Finish()

	elapsed := time.Since(startTime)
	fmt.Printf("\n✅ Done: %d files, %s copied in %s (%.2f MB/s)\n",
		processedCount,
		humanSize(atomic.LoadInt64(&totalBytes)),
		humanDuration(elapsed),
		(float64(atomic.LoadInt64(&totalBytes))/1024/1024)/elapsed.Seconds(),
	)
}

func extractTimestamp(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	x, err := exif.Decode(f)
	if err != nil {
		return time.Time{}, err
	}
	return x.DateTime()
}

func copyFile(src, dst string, modTime time.Time) (int64, error) {
	err := os.MkdirAll(filepath.Dir(dst), 0755)
	if err != nil {
		return 0, err
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	n, err := io.Copy(out, in)
	if err != nil {
		return n, err
	}
	if err := os.Chtimes(dst, modTime, modTime); err != nil {
		return n, err
	}
	return n, nil
}

func loadManifest(path string) map[string]struct{} {
	set := make(map[string]struct{})
	file, err := os.Open(path)
	if err != nil {
		return set
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		set[scanner.Text()] = struct{}{}
	}
	return set
}

func syncManifestWrite(w *bufio.Writer, relPath string) {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	w.WriteString(relPath + "\n")
	w.Flush()
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	if d.Hours() > 1 {
		return fmt.Sprintf("%.0fh%dm", int(d.Hours()), int(d.Minutes())%60)
	} else if d.Minutes() > 1 {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
