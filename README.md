# Mac Manager - A Vibe coded tool

YEAH, It's a shame as a developer using this instead of terminal. But people who all are using systems with limited Resources this will be helpfull to monitor everything at one place.

A local dashboard for macOS. It tracks programs you installed and lets you manage them from one page:

- Apps in `/Applications` and `~/Applications` (Apple system apps are hidden), with running status, CPU, and memory
- Homebrew packages you installed, casks, and `brew services` (start, stop, restart, remove)
- Every Docker container, including stopped ones (start, stop, restart)
- Your processes, separate from macOS background processes

The dashboard binds only to `127.0.0.1`. Docker and Homebrew are optional. If either is missing, its section explains that and the rest of the page still loads.

## Run

```sh
go run .
```

Open http://localhost:8787 in your browser. Use another port with `go run . 9090`.

## Notes

- Quit, Stop, and process stop send `SIGTERM`, so the program can exit cleanly.
- Remove uninstalls a Homebrew formula you installed (`brew leaves`) or an installed cask. Dependencies and config files in your home folder are left alone.
- It does not offer force-kill, elevated privileges, or stop controls for PID 1 or the dashboard itself.
