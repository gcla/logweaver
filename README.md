# Logweaver

A simple CLI tool to merge log files and display them interleaved in chronological order.

## Features

- Automatically extracts timestamps from logs
- Uses terminal colors to help distinguish different logs
- Switches allow you to customize the output format, including "tail -F"
- Transparent support for gzipped log files
- Recursively process log files within a given directory (e.g. for supportsave)
- Written in Golang, compiles to a single executable. Runs on Unix, Windows.

Caveat - I put this tool together quickly for a specific purpose. It may be too limited for general use! There are *no* test cases...

## Building

Logweaver uses Go modules, so it's best to compile with Go 1.12 or higher. Set `GO111MODULE=on` then run:

```bash
go get github.com/gcla/logweaver/cmd/logweaver
```
Then add ```~/go/bin/``` to your ```PATH```.

### From Source

If you want to rebuild the built-in rules too, you can compile from source. First fetch `statik`, a tool for embedding assets in Go binaries

```bash
go get github.com/rakyll/statik
```

Then rebuild logweaver:

```bash
git clone https://github.com/gcla/logweaver
cd logweaver
go generate ./...
go install ./...
```

## Quick Start

Merge `syslog` and `auth.log` chronologically:

```bash
logweaver /var/log/syslog /var/log/auth.log
```

Only show log lines after October 15th at noon:

```bash
logweaver -d --after="October 15th 2020, 12:00" /var/log/syslog /var/log/auth.log
```

Make output look like `tail -F` - no timestamp prefix, log filename appears above log lines:

```bash
logweaver -F /var/log/syslog /var/log/auth.log
```

Similar to `tail -F` except timestamp is extracted and printed normalized as a prefix:

```bash
logweaver -G /var/log/syslog /var/log/auth.log
```

Turn off the terminal colors:

```bash
logweaver -c=no /var/log/syslog /var/log/auth.log
```
or
```bash
logweaver /var/log/syslog /var/log/auth.log | less
```

## Customize

Logweaver writes out a user-config file `~/.logweaver.toml`. Edit that file to add rules for your own logs. You need two pieces of information:

- A regex that extracts the full timestamp - where group #1 of the regex is the match (first paren group)
- A Golang format string to parse the timestamp - see https://golang.org/pkg/time/#pkg-constants

## Limitations

- No automatic support for log files in reverse-chronological order
- Timestamp-extraction is driven by built-in regex rules, then a fallback to https://github.com/araddon/dateparse. This may not succeed on your log files. Customization is available via `~/.logweaver.toml`.
- I haven't thought much about timezones.
- There are *no* test cases :-(

