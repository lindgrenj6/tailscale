// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/peterbourgon/ff/v2/ffcli"
	"golang.org/x/time/rate"
	"inet.af/netaddr"
	"tailscale.com/client/tailscale"
)

var fileCmd = &ffcli.Command{
	Name:       "file",
	ShortUsage: "file <cp|get> ...",
	ShortHelp:  "Send or receive files",
	Subcommands: []*ffcli.Command{
		fileCpCmd,
		// TODO: fileGetCmd,
	},
	Exec: func(context.Context, []string) error {
		// TODO(bradfitz): is there a better ffcli way to
		// annotate subcommand-required commands that don't
		// have an exec body of their own?
		return errors.New("file subcommand required; run 'tailscale file -h' for details")
	},
}

var fileCpCmd = &ffcli.Command{
	Name:       "cp",
	ShortUsage: "file cp <files...> <target>:",
	ShortHelp:  "Copy file(s) to a host",
	Exec:       runCp,
	FlagSet: (func() *flag.FlagSet {
		fs := flag.NewFlagSet("cp", flag.ExitOnError)
		fs.StringVar(&cpArgs.name, "name", "", "alternate filename to use, especially useful when <file> is \"-\" (stdin)")
		fs.BoolVar(&cpArgs.verbose, "verbose", false, "verbose output")
		fs.BoolVar(&cpArgs.targets, "targets", false, "list possible file cp targets")
		return fs
	})(),
}

var cpArgs struct {
	name    string
	verbose bool
	targets bool
}

func runCp(ctx context.Context, args []string) error {
	if cpArgs.targets {
		return runCpTargets(ctx, args)
	}
	if len(args) < 2 {
		//lint:ignore ST1005 no sorry need that colon at the end
		return errors.New("usage: tailscale file cp <files...> <target>:")
	}
	files, target := args[:len(args)-1], args[len(args)-1]
	if !strings.HasSuffix(target, ":") {
		return fmt.Errorf("final argument to 'tailscale file cp' must end in colon")
	}
	target = strings.TrimSuffix(target, ":")
	hadBrackets := false
	if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		hadBrackets = true
		target = strings.TrimSuffix(strings.TrimPrefix(target, "["), "]")
	}
	if ip, err := netaddr.ParseIP(target); err == nil && ip.Is6() && !hadBrackets {
		return fmt.Errorf("an IPv6 literal must be written as [%s]", ip)
	} else if hadBrackets && (err != nil || !ip.Is6()) {
		return errors.New("unexpected brackets around target")
	}
	ip, err := tailscaleIPFromArg(ctx, target)
	if err != nil {
		return err
	}

	peerAPIBase, lastSeen, isOffline, err := discoverPeerAPIBase(ctx, ip)
	if err != nil {
		return err
	}
	if isOffline {
		fmt.Fprintf(os.Stderr, "# warning: %s is offline\n", target)
	} else if !lastSeen.IsZero() && time.Since(lastSeen) > lastSeenOld {
		fmt.Fprintf(os.Stderr, "# warning: %s last seen %v ago\n", target, time.Since(lastSeen).Round(time.Minute))
	}

	if len(files) > 1 {
		if cpArgs.name != "" {
			return errors.New("can't use --name= with multiple files")
		}
		for _, fileArg := range files {
			if fileArg == "-" {
				return errors.New("can't use '-' as STDIN file when providing filename arguments")
			}
		}
	}

	for _, fileArg := range files {
		var fileContents io.Reader
		var name = cpArgs.name
		var contentLength int64 = -1
		if fileArg == "-" {
			fileContents = os.Stdin
			if name == "" {
				name, fileContents, err = pickStdinFilename()
				if err != nil {
					return err
				}
			}
		} else {
			f, err := os.Open(fileArg)
			if err != nil {
				return err
			}
			defer f.Close()
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			if fi.IsDir() {
				return errors.New("directories not supported")
			}
			contentLength = fi.Size()
			fileContents = io.LimitReader(f, contentLength)
			if name == "" {
				name = filepath.Base(fileArg)
			}

			if slow, _ := strconv.ParseBool(os.Getenv("TS_DEBUG_SLOW_PUSH")); slow {
				fileContents = &slowReader{r: fileContents}
			}
		}

		dstURL := peerAPIBase + "/v0/put/" + url.PathEscape(name)
		req, err := http.NewRequestWithContext(ctx, "PUT", dstURL, fileContents)
		if err != nil {
			return err
		}
		req.ContentLength = contentLength
		if cpArgs.verbose {
			log.Printf("sending to %v ...", dstURL)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		if res.StatusCode == 200 {
			io.Copy(ioutil.Discard, res.Body)
			res.Body.Close()
			continue
		}
		io.Copy(os.Stdout, res.Body)
		res.Body.Close()
		return errors.New(res.Status)
	}
	return nil
}

func discoverPeerAPIBase(ctx context.Context, ipStr string) (base string, lastSeen time.Time, isOffline bool, err error) {
	ip, err := netaddr.ParseIP(ipStr)
	if err != nil {
		return "", time.Time{}, false, err
	}
	fts, err := tailscale.FileTargets(ctx)
	if err != nil {
		return "", time.Time{}, false, err
	}
	for _, ft := range fts {
		n := ft.Node
		for _, a := range n.Addresses {
			if a.IP != ip {
				continue
			}
			if n.LastSeen != nil {
				lastSeen = *n.LastSeen
			}
			isOffline = n.Online != nil && !*n.Online
			return ft.PeerAPIURL, lastSeen, isOffline, nil
		}
	}
	return "", time.Time{}, false, errors.New("target seems to be running an old Tailscale version")
}

const maxSniff = 4 << 20

func ext(b []byte) string {
	if len(b) < maxSniff && utf8.Valid(b) {
		return ".txt"
	}
	if exts, _ := mime.ExtensionsByType(http.DetectContentType(b)); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// pickStdinFilename reads a bit of stdin to return a good filename
// for its contents. The returned Reader is the concatenation of the
// read and unread bits.
func pickStdinFilename() (name string, r io.Reader, err error) {
	sniff, err := io.ReadAll(io.LimitReader(os.Stdin, maxSniff))
	if err != nil {
		return "", nil, err
	}
	return "stdin" + ext(sniff), io.MultiReader(bytes.NewReader(sniff), os.Stdin), nil
}

type slowReader struct {
	r  io.Reader
	rl *rate.Limiter
}

func (r *slowReader) Read(p []byte) (n int, err error) {
	const burst = 4 << 10
	plen := len(p)
	if plen > burst {
		plen = burst
	}
	if r.rl == nil {
		r.rl = rate.NewLimiter(rate.Limit(1<<10), burst)
	}
	n, err = r.r.Read(p[:plen])
	r.rl.WaitN(context.Background(), n)
	return
}

const lastSeenOld = 20 * time.Minute

func runCpTargets(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return errors.New("invalid arguments with --targets")
	}
	fts, err := tailscale.FileTargets(ctx)
	if err != nil {
		return err
	}
	for _, ft := range fts {
		n := ft.Node
		var detail string
		if n.Online != nil {
			if !*n.Online {
				detail = "offline"
			}
		} else {
			detail = "unknown-status"
		}
		if detail != "" && n.LastSeen != nil {
			d := time.Since(*n.LastSeen)
			detail += fmt.Sprintf("; last seen %v ago", d.Round(time.Minute))
		}
		if detail != "" {
			detail = "\t" + detail
		}
		fmt.Printf("%s\t%s%s\n", n.Addresses[0].IP, n.ComputedName, detail)
	}
	return nil
}