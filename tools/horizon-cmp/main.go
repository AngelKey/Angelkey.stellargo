package main

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	client "github.com/stellar/go/clients/horizonclient"
	protocol "github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/support/errors"
	slog "github.com/stellar/go/support/log"
	cmp "github.com/stellar/go/tools/horizon-cmp/internal"
)

// maxLevels defines the maximum number of levels deep the crawler
// should go. Here's an example crawl stack:
// Level 1 = /ledgers?order=desc (finds a link to tx details)
// Level 2 = /transactions/abcdef (finds a link to a list of operations)
// Level 3 = /transactions/abcdef/operations (will not follow any links - at level 3)
const maxLevels = 3
const pathsQueueCap = 10000

// pathAccessLog is a regexp that gets path from ELB access log line. Example:
// 2015-05-13T23:39:43.945958Z my-loadbalancer 192.168.131.39:2817 10.0.0.1:80 0.000086 0.001048 0.001337 200 200 0 57 "GET https://www.example.com:443/transactions?order=desc HTTP/1.1" "curl/7.38.0" DHE-RSA-AES128-SHA TLSv1.2
var pathAccessLog = regexp.MustCompile(`([A-Z]+) https?://[^/]*(/[^ ]*)`)

var (
	paths        = make(chan cmp.Path, pathsQueueCap)
	visitedPaths map[string]bool
)

// CLI params
var (
	horizonBase           string
	horizonTest           string
	elbAccessLogFile      string
	elbAccessLogStartLine int
	requestsPerSecond     int
)

var log *slog.Entry

var rootCmd = &cobra.Command{
	Use:   "horizon-cmp",
	Short: "horizon-cmp compares two horizon servers' responses",
	Run: func(cmd *cobra.Command, args []string) {
		run(cmd)
	},
}

func init() {
	log = slog.New()
	log.SetLevel(slog.InfoLevel)
	log.Logger.Formatter.(*logrus.TextFormatter).DisableTimestamp = true

	if cap(paths) < len(initPaths) {
		panic("cap(paths) must be higher or equal len(initPaths)")
	}

	visitedPaths = make(map[string]bool)

	rootCmd.Flags().StringVarP(&horizonBase, "base", "b", "", "URL of the base/old version Horizon server")
	rootCmd.Flags().StringVarP(&horizonTest, "test", "t", "", "URL of the test/new version Horizon server")
	rootCmd.Flags().StringVarP(&elbAccessLogFile, "elb-access-log-file", "a", "", "ELB access log file to replay")
	rootCmd.Flags().IntVarP(&elbAccessLogStartLine, "elb-access-log-start-line", "s", 1, "Start line of ELB access log (useful to continue from a given point)")
	rootCmd.Flags().IntVar(&requestsPerSecond, "rps", 1, "Requests per second")
}

func main() {
	rootCmd.Execute()
}

func run(cmd *cobra.Command) {
	if horizonBase == "" || horizonTest == "" {
		log.Error("--base and --test params are required")
		cmd.Help()
		os.Exit(1)
	}

	// Get latest ledger and operate on it's cursor to get responses at a given ledger.
	ledger := getLatestLedger()
	cursor := ledger.PagingToken()

	var accessLog *cmp.Scanner
	if elbAccessLogFile == "" {
		for _, p := range initPaths {
			paths <- cmp.Path{Path: getPathWithCursor(p, cursor), Level: 0, Stream: false}
			paths <- cmp.Path{Path: getPathWithCursor(p, cursor), Level: 0, Stream: true}
		}
	} else {
		file, err := os.Open(elbAccessLogFile)
		if err != nil {
			panic(err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		accessLog = &cmp.Scanner{Scanner: scanner}
		// Seek
		if elbAccessLogStartLine > 1 {
			log.Info("Seeking file...")
		}
		for i := 1; i < elbAccessLogStartLine; i++ {
			accessLog.Scan()
		}
		// Streams lines to channel from another go routine
		go streamFile(accessLog)
	}

	pwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	outputDir := fmt.Sprintf("%s/horizon-cmp-diff/%d", pwd, time.Now().Unix())

	log.WithFields(slog.F{
		"base":       horizonBase,
		"test":       horizonTest,
		"access_log": elbAccessLogFile,
		"ledger":     ledger.Sequence,
		"cursor":     cursor,
		"output_dir": outputDir,
	}).Info("Starting...")

	err = os.MkdirAll(outputDir, 0744)
	if err != nil {
		panic(err)
	}

	var wg sync.WaitGroup
	for {
		pl, more := <-paths
		if !more {
			break
		}

		if pl.Level > maxLevels {
			continue
		}

		if visitedPaths[pl.ID()] {
			continue
		}
		visitedPaths[pl.ID()] = true

		time.Sleep(time.Second / time.Duration(requestsPerSecond))
		wg.Add(1)
		go func() {
			defer wg.Done()

			a := cmp.NewResponse(horizonBase, pl.Path, pl.Stream)
			b := cmp.NewResponse(horizonTest, pl.Path, pl.Stream)

			var status string
			if a.Equal(b) {
				status = "ok"
			} else {
				status = "diff"
				a.SaveDiff(outputDir, b)
			}

			log = log.WithFields(slog.F{
				"status_code": a.StatusCode,
				"size_base":   a.Size(),
				"size_test":   b.Size(),
				"stream":      pl.Stream,
			})

			if accessLog != nil {
				log = log.WithField("access_log_line", pl.Line)
			}

			if status == "diff" {
				log.Error("DIFF " + pl.Path)
			} else {
				log.Info(pl.Path)
			}

			// Add new paths (only for non-ELB)
			if accessLog == nil {
				addPathsFromResponse(a, pl.Level+1)
			}
		}()
	}

	wg.Wait()
}

func getLatestLedger() protocol.Ledger {
	horizon := client.Client{
		HorizonURL: horizonBase,
		HTTP:       http.DefaultClient,
	}

	ledgers, err := horizon.Ledgers(client.LedgerRequest{
		Order: client.OrderDesc,
		Limit: 1,
	})

	if err != nil {
		panic(err)
	}

	return ledgers.Embedded.Records[0]
}

func getPathWithCursor(path, cursor string) string {
	urlObj, err := url.Parse(path)
	if err != nil {
		panic(err)
	}

	// Add cursor if not present
	q := urlObj.Query()
	if q.Get("cursor") == "" {
		q.Set("cursor", cursor)
	}

	urlObj.RawQuery = q.Encode()
	return urlObj.String()
}

func getPathFromAccessLog(line string) (string, error) {
	matches := pathAccessLog.FindStringSubmatch(line)
	if len(matches) != 3 {
		return "", errors.Errorf("Can't find match: %s", line)
	}

	if matches[1] != "GET" {
		return "", nil
	}

	return matches[2], nil
}

func streamFile(accessLog *cmp.Scanner) {
	for accessLog.Scan() {
		p := accessLog.Text()
		path, err := getPathFromAccessLog(p)
		if err != nil {
			log.Error(err)
			continue
		}

		if path == "" {
			continue
		}

		paths <- cmp.Path{Path: path, Level: 0, Line: accessLog.LinesRead(), Stream: false}
		paths <- cmp.Path{Path: path, Level: 0, Line: accessLog.LinesRead(), Stream: true}
	}

	if err := accessLog.Err(); err != nil {
		panic("Invalid input: " + err.Error())
	}

	close(paths)
}

func addPathsFromResponse(a *cmp.Response, level int) {
	newPaths := a.GetPaths()
	for _, newPath := range newPaths {
		// For all indexes with chronological sort ignore order=asc
		// without cursor. There will always be a diff if Horizon started
		// at a different ledger.
		if strings.Contains(newPath, "/ledgers") ||
			strings.Contains(newPath, "/transactions") ||
			strings.Contains(newPath, "/operations") ||
			strings.Contains(newPath, "/payments") ||
			strings.Contains(newPath, "/effects") ||
			strings.Contains(newPath, "/trades") {
			u, err := url.Parse(newPath)
			if err != nil {
				panic(err)
			}

			if u.Query().Get("cursor") == "" &&
				(u.Query().Get("order") == "" || u.Query().Get("order") == "asc") {
				continue
			}
		}

		if (strings.Contains(newPath, "/transactions") ||
			strings.Contains(newPath, "/operations") ||
			strings.Contains(newPath, "/payments")) && !strings.Contains(newPath, "include_failed") {
			prefix := "?"
			if strings.Contains(newPath, "?") {
				prefix = "&"
			}

			paths <- cmp.Path{newPath + prefix + "include_failed=false", level, 0, false}
			paths <- cmp.Path{newPath + prefix + "include_failed=false", level, 0, true}

			paths <- cmp.Path{newPath + prefix + "include_failed=true", level, 0, false}
			paths <- cmp.Path{newPath + prefix + "include_failed=true", level, 0, true}
			continue
		}

		paths <- cmp.Path{newPath, level, 0, false}
		paths <- cmp.Path{newPath, level, 0, true}
	}

	if len(paths) == 0 {
		close(paths)
	}
}
