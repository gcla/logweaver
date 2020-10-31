package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/araddon/dateparse"
	flags "github.com/jessevdk/go-flags"
	"github.com/lestrrat-go/strftime"
	"github.com/logrusorgru/aurora"
	"github.com/rakyll/statik/fs"

	_ "github.com/gcla/logweaver/assets/statik"
)

var (
	timestampFormatDefault string = "%a %T"
	timestampFormatShort   string = "%T"
	timestampFormatLong    string = "%d/%b/%Y:%H:%M:%S %z"
	helpHeader             string = `logweaver v0.6
Combine log files together in chronological order.

`
	helpFooter string = fmt.Sprintf(`
The default timestamp format is %s.
See https://strftime.org/ for timestamp format syntax.
`, strings.ReplaceAll(timestampFormatDefault, "%", "%%"))
)

type Flags struct {
	Help                  bool     `long:"help" short:"h" optional:"true" optional-value:"true" description:"Show this help message."`
	UseFullname           bool     `long:"show-path" short:"f" optional:"true" optional-value:"true" description:"Use full path of log file in output."`
	TimeFormat1           bool     `long:"full-timestamp" short:"1" description:"Use a fuller timestamp format (%d/%b/%Y:%H:%M:%S %z)."`
	TimeFormat2           bool     `long:"short-timestamp" short:"2" description:"Use a short timestamp format (%T)."`
	TimeFormat            string   `long:"time-format" short:"t" description:"strftime-compatible string to use when printing out timestamps."`
	DontReplaceTimestamp  bool     `long:"dont-replace-timestamp" short:"d" optional:"true" optional-value:"true" description:"Don't replace timestamps in log file output."`
	ReplaceTimestampToken string   `long:"timestamp-replacement" short:"r" optional:"false" default:"<T>" description:"Use this token instead of a timestamp for narrower output."`
	NoTimestamp           bool     `long:"no-timestamp" short:"n" optional:"true" optional-value:"true" description:"Don't prefix the line with the normalized timestamp."`
	Color                 TriState `long:"color" short:"c" optional:"true" optional-value:"true" default:"unset" description:"Use terminal colors."`
	ColorEnv              TriState `long:"color-env" hidden:"true" env:"LOGWEAVER_USE_COLOR" description:"Use terminal colors (internal use)."`
	ShowUserConfig        bool     `long:"show-user-config" optional:"true" optional-value:"true" description:"Show the user's configuration as TOML."`
	ShowDefaultConfig     bool     `long:"show-default-config" optional:"true" optional-value:"true" description:"Show the default built-in configuration as TOML."`
	TailStyle             bool     `long:"tail-F-style" short:"F" optional:"true" optional-value:"true" description:"Use tail-F style output."`
	AltStyle              bool     `long:"alt-style" short:"G" optional:"true" optional-value:"true" description:"Log file on a separate line; time-stamp is a prefix."`
	Separator             bool     `long:"separator" short:"s" optional:"true" optional-value:"true" description:"Print a separator between different log files."`
	After                 *string  `long:"after" short:"a" optional:"false" description:"Show only log entries after this point in time."`
	TimeZone              string   `long:"timezone" short:"z" optional:"true" default:"UTC" description:"Display timestamps relative to this timezone."`
	Logs                  struct {
		FilesAndDirs []string `value-name:"<files-and-dirs>" description:"Log files to process. Directories read recursively."`
	} `positional-args:"yes"`
}

type Config struct {
	Match []Match
}

type Match struct {
	Match  string
	Format string
	re     *regexp.Regexp
}

type State struct {
	filename       string         // e.g. /var/log/keepalived.log
	basename       string         // e.g. keepalived.log (compute once)
	scanner        *bufio.Scanner // for reading the log file line by line
	line           string         // the current line, maybe with the timestamp replaced by a short token
	eof            bool           // true if we've reached eof - log file will then be dropped by main loop
	haveLine       bool           // false if next time round the loop, we need to scan for a new line (i.e. we just processed the last line)
	reIdx          int            // != -1 means we have figured out which regex to use to extract the timestamp for this file
	newEnough      bool           // true if the log lines are now newer than the time in the --after flag
	tm             time.Time      // the computed timestamp for the current line
	continuation   bool           // true if this line is a continuation of the previous line's log message
	warnedSkipping bool           // if true, then we found unparseable lines at the beginning, and HAVE printed a warning about it
	color          uint8          // if not nil, the color to use when emitting a log line for this file
}

// We'll keep a list of these to open, computed from command line arguments
type LogFileArg struct {
	name        string
	notRequired bool
	handle      *os.File
}

type LogFileArgs []LogFileArg

func (f LogFileArgs) Close() error {
	var err error
	for _, a := range f {
		err2 := a.handle.Close()
		if err2 != nil {
			err = err2
		}
	}
	return err
}

type TriState struct {
	Set bool
	Val bool
}

func (b *TriState) UnmarshalFlag(value string) error {
	switch value {
	case "true", "TRUE", "t", "T", "1", "y", "Y", "yes", "Yes", "YES":
		b.Set = true
		b.Val = true
	case "false", "FALSE", "f", "F", "0", "n", "N", "no", "No", "NO":
		b.Set = true
		b.Val = false
	default:
		b.Set = false
	}
	return nil
}

func (b TriState) MarshalFlag() string {
	if b.Set {
		if b.Val {
			return "true"
		} else {
			return "false"
		}
	} else {
		return "unset"
	}
}

func writeHelp(f *flags.Parser, w io.Writer) {
	fmt.Fprintf(w, helpHeader)
	f.WriteHelp(w)
	fmt.Fprintf(w, helpFooter)
}

func main() {
	res := cmain()
	os.Exit(res)
}

func cmain() int {

	var opts Flags

	flags := flags.NewParser(&opts, 0)
	_, err := flags.ParseArgs(os.Args)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
		writeHelp(flags, os.Stderr)
		return 1
	}

	if opts.Help {
		writeHelp(flags, os.Stdout)
		return 0
	}

	if len(opts.Logs.FilesAndDirs) <= 1 && !opts.ShowDefaultConfig && !opts.ShowUserConfig {
		fmt.Fprintf(os.Stderr, "Please specify files or directories to process.\n\n")
		writeHelp(flags, os.Stderr)
		return 1
	}

	if opts.ShowDefaultConfig && opts.ShowUserConfig {
		fmt.Fprintf(os.Stderr, "Please choose to show either the user config or the default config.\n\n")
		writeHelp(flags, os.Stderr)
		return 1
	}

	if opts.TimeFormat != "" && opts.TimeFormat1 {
		fmt.Fprintf(os.Stderr, "Please choose only one timestamp format.\n\n")
		writeHelp(flags, os.Stderr)
		return 1
	}

	loc, err := time.LoadLocation(opts.TimeZone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error interpreting '%s' as a timezone: %v\n\n", opts.TimeZone, err)
		writeHelp(flags, os.Stderr)
		return 1
	}

	var startAfter time.Time // starts at time 0, so we can always compare to this if it's not explicitly set
	if opts.After != nil {
		startAfter, err = dateparse.ParseAny(*opts.After)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: did not understand --after argument '%s': %v\n", *opts.After, err)
			return 1
		}
	}

	// Since tail-F style implies no timestamp prefix, we shouldn't replace the timestamp token
	// or there'll be no way for the user to see it (without manually adding this flag which is
	// a poor default)
	if opts.TailStyle {
		opts.DontReplaceTimestamp = true
	}

	if !opts.Color.Set && opts.ColorEnv.Set {
		opts.Color.Set = true
		opts.Color.Val = opts.ColorEnv.Val
	}

	// Used for the timestamp prefix
	var timeFmt *strftime.Strftime
	if opts.TimeFormat1 {
		timeFmt, _ = strftime.New(timestampFormatLong)
	} else if opts.TimeFormat2 {
		timeFmt, _ = strftime.New(timestampFormatShort)
	} else if opts.TimeFormat != "" {
		timeFmt, err = strftime.New(opts.TimeFormat)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s is an invalid format: %v\n", opts.TimeFormat, err)
			return 1
		}
	} else {
		// Default
		timeFmt, _ = strftime.New(timestampFormatDefault)
	}

	usr, err := user.Current()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not access home directory: %v\n", err)
		return 1
	}

	statikFS, err := fs.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	// The user's config file takes precedence
	userConfigPath := filepath.Join(usr.HomeDir, ".logweaver.toml")
	_, err = os.Stat(userConfigPath)
	if os.IsNotExist(err) {
		err = func() error {
			// Set up the default empty user config ~/.logweaver.toml
			r, err := statikFS.Open("/empty.toml")
			if err != nil {
				return err
			}
			defer r.Close()

			destination, err := os.Create(userConfigPath)
			if err != nil {
				return err
			}
			defer destination.Close()
			_, err = io.Copy(destination, r)
			return err
		}()

		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", userConfigPath, err)
			return 1
		}
	}

	userConfig, err := os.Open(userConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening config %s: %v\n", userConfigPath, err)
		return 1
	}
	defer userConfig.Close()

	defaultConfig, err := statikFS.Open("/logweaver.toml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unexpected error reading default config: %v\n", err)
		return 1
	}
	defer defaultConfig.Close()

	var conf Config
	if !opts.ShowDefaultConfig && !opts.ShowUserConfig {
		for _, confReader := range []io.Reader{userConfig, defaultConfig} {
			var localConf Config

			_, err = toml.DecodeReader(confReader, &localConf)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error decoding toml: %v\n", err)
				return 1
			}

			for i, m := range localConf.Match {
				localConf.Match[i].re, err = regexp.Compile(m.Match)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error opening parsing regex %s: %v\n", m.Match, err)
					return 1
				}
			}
			conf.Match = append(conf.Match, localConf.Match...)
		}
	}

	// Returns true after pipeline completes, if fork/exec under /bin/sh is possible. In
	// which case, return early from main because the pipeline is doing the real work. If
	// false, it means something went wrong, or fork/exec is not possible (windows).
	if maybeExecWithPager(!opts.Color.Set || opts.Color.Val) {
		return 0
	}

	// Do this later so we can benefit from the pager
	if opts.ShowDefaultConfig {
		io.Copy(os.Stdout, defaultConfig)
		return 0
	} else if opts.ShowUserConfig {
		io.Copy(os.Stdout, userConfig)
		return 0
	}

	longestLen := -1
	var reader io.Reader

	// Overall approach is to start with logFileArgsPre, which might contain directories, then
	// flatten as we transfer for logFileArgs.
	var logFileArgsPre LogFileArgs = make([]LogFileArg, 0, 128)
	var logFileArgs LogFileArgs = make([]LogFileArg, 0, 128)
	defer logFileArgs.Close()

	// build up Pre-list from CLI args
	for _, arg := range opts.Logs.FilesAndDirs[1:] {
		logFileArgsPre = append(logFileArgsPre, LogFileArg{name: arg}) // all are required
	}

	// Process these, in LIFO order. Each file gets moved to logFileArgs. Each dir is
	// walked recursively, adding to the beginning of logFileArgsPre, growing it. When
	// finished, logFileArgs will be a (maybe long) list of all log files to process.
	for len(logFileArgsPre) > 0 {
		cur := logFileArgsPre[0]
		logFileArgsPre = logFileArgsPre[1:]

		cur.handle, err = os.Open(cur.name)
		if err != nil {
			if !cur.notRequired {
				fmt.Fprintf(os.Stderr, "Error opening log file %s: %v\n", cur.name, err)
				return 1
			} else {
				fmt.Fprintf(os.Stderr, "Warning, problem issuing stat on %s: %v\n", cur.name, err)
				continue
			}
		}
		fi, err := cur.handle.Stat()
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "Could not stat file %s: %v\n", cur.name, err)
			return 1
		case fi.IsDir():
			cur.handle.Close()
			err = filepath.Walk(cur.name, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if !info.IsDir() {
					logFileArgsPre = append(logFileArgsPre, LogFileArg{
						name:        path,
						notRequired: true,
					})
				}
				return nil
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error scanning directory %s: %v\n", cur.name, err)
				return 1
			}
		default:
			logFileArgs = append(logFileArgs, cur)
		}
	}

	state := make([]State, 0)
	// Now we have all files in this list. All files will have open handles. Compute the longest filename
	// length - for formatting neatly - and build up our initial state.
	for _, arg := range logFileArgs {
		// For formatting
		if !opts.UseFullname {
			if len(filepath.Base(arg.name)) > longestLen {
				longestLen = len(filepath.Base(arg.name))
			}
		} else {
			if len(arg.name) > longestLen {
				longestLen = len(arg.name)
			}
		}

		breader := bufio.NewReader(arg.handle)
		reader = breader

		testBytes, err := breader.Peek(2) //read 2 bytes
		if err == nil && testBytes[0] == 31 && testBytes[1] == 139 {
			greader, err := gzip.NewReader(breader)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error opening gzip compressed file %s: %v\n", arg, err)
				return 1
			}
			defer greader.Close()
			reader = greader
		}

		state = append(state, State{
			filename: arg.name,
			basename: filepath.Base(arg.name),
			scanner:  bufio.NewScanner(reader),
			reIdx:    -1,
		})
	}

	var prefixFormat string
	var separator string
	switch {
	case opts.NoTimestamp && opts.AltStyle:
		// "%s"
		prefixFormat = "%s\n"
		// ========================
		separator = fmt.Sprintf(prefixFormat,
			strings.Repeat("=", 40),
		)
	case opts.NoTimestamp && !opts.AltStyle:
		// "%s | %-20s | %s
		prefixFormat = fmt.Sprintf("%%-%ds", longestLen) + " | %s\n"
		// ==================== | ========================
		separator = fmt.Sprintf(prefixFormat,
			strings.Repeat("=", longestLen),
			strings.Repeat("=", 40),
		)
	case !opts.NoTimestamp && opts.AltStyle:
		// "%s | %s
		prefixFormat = "%s | %s\n"
		// ======== | ==================== | ========================
		separator = fmt.Sprintf(prefixFormat,
			strings.Repeat("=", len(timeFmt.FormatString(time.Now()))),
			strings.Repeat("=", 40),
		)
	case !opts.NoTimestamp && !opts.AltStyle:
		// "%s | %-20s | %s
		prefixFormat = "%s | " + fmt.Sprintf("%%-%ds", longestLen) + " | %s\n"
		// ======== | ==================== | ========================
		separator = fmt.Sprintf(prefixFormat,
			strings.Repeat("=", len(timeFmt.FormatString(time.Now()))),
			strings.Repeat("=", longestLen),
			strings.Repeat("=", 40),
		)
	}

	parseTimestampFromMatch := func(match *Match, line *string) (time.Time, error) {
		var t time.Time
		var err error
		guess := true
		if match.Format != "" {
			guess = false
			t, err = time.Parse(match.Format, *line)
			if err == nil {
				if t.Year() == 0 {
					t = t.AddDate(time.Now().Year(), 0, 0)
				}
			} else {
				guess = true
			}
		}
		if guess {
			t, err = dateparse.ParseAny(*line)
		}
		return t, err
	}

	var foundTimestampInLine bool
	var tm time.Time
	var tmArg string
	var logFileArg interface{}
	var lastFile string // keep track of the last file that generated output, so we know if we need a separator

	colors := 0
	if opts.Color.Set && opts.Color.Val {
		colors = 8 // stick to 8 - the other colors have a lot of darks which become hard to see in a black background
	}

	cur := 0
	for i := 0; i < len(state); i++ {
		if cur == 0 {
			if colors > 8 {
				cur = 9 // skip dark colors
			} else {
				cur = 1 // skip black
			}
		} else if cur == 7 {
			cur = 1
		}
		line := fmt.Sprintf("Including file %s\n", state[i].filename)
		if colors > 0 {
			state[i].color = uint8(cur % colors)
			fmt.Printf("%s", aurora.Index(state[i].color, line))
		} else {
			fmt.Printf(line)
		}
		cur += 1
	}
	fmt.Println()

	matches := make([]int, 0, 16)
	lineArgs := make([]interface{}, 0, 8)

	var s *State

	for {
		for si, _ := range state {
			s = &state[si]
			if !s.haveLine && !s.eof {
			lineloop:
				// loop to possibly skip lines at the start of a file (start meaning before we've had a positive match
				// that fixes the regex to use)
				for {
					state[si].eof = !s.scanner.Scan()
					if state[si].eof {
						break
					} else {
						state[si].line = s.scanner.Text()
						state[si].haveLine = true

						foundTimestampInLine = false
						if state[si].reIdx != -1 { // means we know which regex to use now
							matches = conf.Match[state[si].reIdx].re.FindStringSubmatchIndex(state[si].line)
							if len(matches) >= 4 {
								ln := state[si].line[matches[2]:matches[3]]
								tm, err = parseTimestampFromMatch(&conf.Match[state[si].reIdx], &ln)
								if err == nil {
									if tm.After(startAfter) {
										state[si].newEnough = true
										foundTimestampInLine = true
										if tm.Before(state[si].tm) {
											// This is a strange case - here's an example:
											//
											// [2020-10-05 16:06:40] systemctl status mariadb -l --no-pager
											// ● mariadb.service - MariaDB 10.4.13 database server
											//    Loaded: loaded (/lib/systemd/system/mariadb.service; enabled; vendor preset: enabled)
											//   Drop-In: /etc/systemd/system/mariadb.service.d
											//            └─migrated-from-my.cnf-settings.conf
											//    Active: active (running) since Mon 2020-10-05 16:05:55 CEST; 45s ago
											//      Docs: man:mysqld(8)
											//            https://mariadb.com/kb/en/library/systemd/
											//  Main PID: 5813 (mysqld)
											//    Status: "Taking your SQL requests now..."
											//     Tasks: 37 (limit: 4638)
											//    CGroup: /system.slice/mariadb.service
											//            └─5813 /usr/sbin/mysqld --wsrep-new-cluster --wsrep_start_position=00000000-0000-0000-0000-000000000000:-1
											//
											// Oct 05 16:06:15 tpvm1 -innobackupex-backup[6193]: [00] 2020-10-05 16:06:15 All tables unlocked
											// Oct 05 16:06:15 tpvm1 -innobackupex-backup[6193]: [00] 2020-10-05 16:06:15 Streaming ib_buffer_pool to <STDOUT>
											// Oct 05 16:06:15 tpvm1 -innobackupex-backup[6193]: [00] 2020-10-05 16:06:15         ...done
											// Oct 05 16:06:15 tpvm1 -innobackupex-backup[6193]: [00] 2020-10-05 16:06:15 Backup created in directory '/tmp/tmp.94x28Gk7N2/'
											//
											// The lowest lines are the output from systemctl status mariadb, and are just appended to the log. They
											// have parseable timestamps, but they don't represent legitimate log entries. Because they'd always be
											// earlier in time than the introducing log line, we can assume they should be treated as continuations.
											// Note that this example wouldn't show this problem precisely, because the regex to match the introducing
											// line would not match the false log files in the systemctl output. But they could, in principle.
											state[si].continuation = true
										} else {
											state[si].tm = tm
										}
										if !opts.DontReplaceTimestamp {
											state[si].line = state[si].line[0:matches[2]] + opts.ReplaceTimestampToken + state[si].line[matches[3]:]
										}
									}
								}
							}
							if si == 0 && !foundTimestampInLine && state[si].newEnough {
								// this was the last file to emit a line (because it's sorted to the front). We
								// assume if we can't parse the line, then it just belongs to the current file
								// Use last timestamp for this file by default (don't change .tm)
								state[si].continuation = true
								foundTimestampInLine = true
							}
						} else {
							// we don't know which regex to use yet for this file, so try them all
							for mi, match := range conf.Match {
								matches = match.re.FindStringSubmatchIndex(state[si].line)
								if len(matches) >= 4 {
									ln := state[si].line[matches[2]:matches[3]]
									tm, err = parseTimestampFromMatch(&match, &ln)

									if err == nil {
										state[si].reIdx = mi
										if tm.After(startAfter) {
											foundTimestampInLine = true
											state[si].newEnough = true
											state[si].tm = tm
											if !opts.DontReplaceTimestamp {
												state[si].line = state[si].line[0:matches[2]] + opts.ReplaceTimestampToken + state[si].line[matches[3]:]
											}
										}
										break
									}
								}
							}
						}

						if foundTimestampInLine { // means we can stop skipping, if we were
							break lineloop
						} else {
							// What if we get here and s.reIdx != -1 and si != 0
							// That means for this file, we already have an established regex; but this file
							// isn't the first in the sorted list.
							// That state can't happen - we only fetch a new line from a file when we've emitted
							// the previous line, and if we've just emitted this file's line, then si == 0. If
							// we can't find a timestamp, we assume it's a continuation and just print it out
							// this time, emitting all such lines until we get a regex hit again.
							// So if we get here without a timestamp, it means we're still trying all regexes
							// to look for the first one that matches anything. In that case, we've not emitted
							// anything yet for this file - so we can just print a warning, and skip this line
							// and all subsequent until we can extract a timestamp.
							if state[si].reIdx == -1 && !state[si].warnedSkipping {
								fmt.Fprintf(os.Stdout, "Warning: skipping unparsed lines from start of %s...\n", s.filename)
								state[si].warnedSkipping = true
							}
						}

					}
				}
			}
		}

		// Remove all log files that we've reached the end of.
		var cur int
		for _, st := range state {
			if st.eof {
				continue
			}
			state[cur] = st
			cur++
		}
		state = state[:cur]

		if len(state) == 0 {
			break
		}

		// The earliest timestamp should come first; or any file that has "continued"
		// lines that need to be emitted because they're associated with the last line
		// emitted.
		sort.Slice(state, func(i, j int) bool {
			return state[i].continuation || state[i].tm.Before(state[j].tm)
		})

		// Normalize to UTC
		tmArg = timeFmt.FormatString(state[0].tm.In(loc))

		if state[0].continuation || lastFile == state[0].filename {
			logFileArg = ""
		} else {
			if !opts.UseFullname {
				logFileArg = state[0].basename
			} else {
				logFileArg = state[0].filename
			}
		}
		if !opts.TailStyle && opts.Separator && lastFile != "" && lastFile != state[0].filename {
			// A separator not a header, so don't emit for the first file
			if colors > 0 {
				fmt.Printf("%s", aurora.Index(state[0].color, separator))
			} else {
				fmt.Printf(separator)
			}
		}
		if opts.TailStyle || opts.AltStyle {
			// More of a header than separator, so print out for the first file
			if lastFile != state[0].filename {
				printme := fmt.Sprintf("\n==> %s <==\n", logFileArg)
				if colors > 0 {
					fmt.Printf("%s", aurora.Index(state[0].color, printme))
				} else {
					fmt.Printf(printme)
				}
			}
		}
		if opts.TailStyle {
			if colors > 0 {
				fmt.Printf("%s\n", aurora.Index(state[0].color, state[0].line))
			} else {
				fmt.Printf("%s\n", state[0].line)
			}
		} else {
			lineArgs = lineArgs[:0]
			if !opts.NoTimestamp {
				lineArgs = append(lineArgs, tmArg)
			}
			if !opts.AltStyle {
				lineArgs = append(lineArgs, logFileArg)
			}
			lineArgs = append(lineArgs, state[0].line)
			printme := fmt.Sprintf(prefixFormat, lineArgs...)
			if colors > 0 {
				fmt.Printf("%s", aurora.Index(state[0].color, printme))
			} else {
				fmt.Printf(printme)
			}
		}

		lastFile = state[0].filename
		state[0].haveLine = false
		state[0].continuation = false
	}

	return 0
}
