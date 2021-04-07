package watch

import (
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/cli/cli/api"
	"github.com/cli/cli/internal/ghrepo"
	"github.com/cli/cli/pkg/cmd/run/shared"
	"github.com/cli/cli/pkg/cmdutil"
	"github.com/cli/cli/pkg/iostreams"
	"github.com/cli/cli/utils"
	"github.com/spf13/cobra"
)

const defaultInterval int = 3

type WatchOptions struct {
	IO         *iostreams.IOStreams
	HttpClient func() (*http.Client, error)
	BaseRepo   func() (ghrepo.Interface, error)

	RunID      string
	Interval   int
	ExitStatus bool

	Prompt bool

	Now func() time.Time
}

func NewCmdWatch(f *cmdutil.Factory, runF func(*WatchOptions) error) *cobra.Command {
	opts := &WatchOptions{
		IO:         f.IOStreams,
		HttpClient: f.HttpClient,
		Now:        time.Now,
	}

	cmd := &cobra.Command{
		Use:   "watch <run-selector>",
		Short: "Runs until a run completes, showing its progress",
		Annotations: map[string]string{
			"IsActions": "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// support `-R, --repo` override
			opts.BaseRepo = f.BaseRepo

			if len(args) > 0 {
				opts.RunID = args[0]
			} else if !opts.IO.CanPrompt() {
				return &cmdutil.FlagError{Err: errors.New("run ID required when not running interactively")}
			} else {
				opts.Prompt = true
			}

			if runF != nil {
				return runF(opts)
			}

			return watchRun(opts)
		},
	}
	cmd.Flags().BoolVar(&opts.ExitStatus, "exit-status", false, "Exit with non-zero status if run fails")
	cmd.Flags().IntVarP(&opts.Interval, "interval", "i", defaultInterval, "Refresh interval in seconds")

	return cmd
}

func watchRun(opts *WatchOptions) error {
	c, err := opts.HttpClient()
	if err != nil {
		return fmt.Errorf("failed to create http client: %w", err)
	}
	client := api.NewClientFromHTTP(c)

	repo, err := opts.BaseRepo()
	if err != nil {
		return fmt.Errorf("failed to determine base repo: %w", err)
	}

	runID := opts.RunID
	var run *shared.Run

	if opts.Prompt {
		cs := opts.IO.ColorScheme()
		runs, err := shared.GetRunsWithFilter(client, repo, 10, func(run shared.Run) bool {
			return run.Status != shared.Completed
		})
		if err != nil {
			return fmt.Errorf("failed to get runs: %w", err)
		}
		if len(runs) == 0 {
			return fmt.Errorf("found no in progress runs to watch")
		}
		runID, err = shared.PromptForRun(cs, runs)
		if err != nil {
			return err
		}
		// TODO silly stopgap until dust settles and PromptForRun can just return a run
		for _, r := range runs {
			if fmt.Sprintf("%d", r.ID) == runID {
				run = &r
				break
			}
		}
	} else {
		run, err = shared.GetRun(client, repo, runID)
		if err != nil {
			return fmt.Errorf("failed to get run: %w", err)
		}
	}

	if run.Status == shared.Completed {
		return nil
	}

	prNumber := ""
	number, err := shared.PullRequestForRun(client, repo, *run)
	if err == nil {
		prNumber = fmt.Sprintf(" #%d", number)
	}

	if runtime.GOOS == "windows" {
		opts.IO.EnableVirtualTerminalProcessing()
	}
	// clear entire screen
	fmt.Fprintf(opts.IO.Out, "\x1b[2J")

	annotationCache := map[int][]shared.Annotation{}

	duration := time.Second * opts.Interval

	for run.Status != shared.Completed {
		run, err = renderRun(*opts, client, repo, run, prNumber, annotationCache)
		if err != nil {
			return err
		}
		time.Sleep(duration)
	}

	if opts.ExitStatus && run.Conclusion != shared.Success {
		return cmdutil.SilentError
	}

	return nil
}

func renderRun(opts WatchOptions, client *api.Client, repo ghrepo.Interface, run *shared.Run, prNumber string, annotationCache map[int][]shared.Annotation) (*shared.Run, error) {
	out := opts.IO.Out
	cs := opts.IO.ColorScheme()

	var err error

	run, err = shared.GetRun(client, repo, fmt.Sprintf("%d", run.ID))
	if err != nil {
		return run, fmt.Errorf("failed to get run: %w", err)
	}

	ago := opts.Now().Sub(run.CreatedAt)

	jobs, err := shared.GetJobs(client, repo, *run)
	if err != nil {
		return run, fmt.Errorf("failed to get jobs: %w", err)
	}

	var annotations []shared.Annotation

	var annotationErr error
	var as []shared.Annotation
	for _, job := range jobs {
		if as, ok := annotationCache[job.ID]; ok {
			annotations = as
			continue
		}

		as, annotationErr = shared.GetAnnotations(client, repo, job)
		if annotationErr != nil {
			break
		}
		annotations = append(annotations, as...)

		if job.Status != shared.InProgress {
			annotationCache[job.ID] = annotations
		}
	}

	if annotationErr != nil {
		return run, fmt.Errorf("failed to get annotations: %w", annotationErr)
	}

	if runtime.GOOS == "windows" {
		// Just clear whole screen; I wasn't able to get the nicer cursor movement thing working
		fmt.Fprintf(opts.IO.Out, "\x1b[2J")
	} else {
		// Move cursor to 0,0
		fmt.Fprint(opts.IO.Out, "\x1b[0;0H")
		// Clear from cursor to bottom of screen
		fmt.Fprint(opts.IO.Out, "\x1b[J")
	}

	fmt.Fprintln(out, cs.Boldf("Refreshing run status every %d seconds. Press Ctrl+C to quit.", opts.Interval))
	fmt.Fprintln(out)
	fmt.Fprintln(out, shared.RenderRunHeader(cs, *run, utils.FuzzyAgo(ago), prNumber))
	fmt.Fprintln(out)

	if len(jobs) == 0 && run.Conclusion == shared.Failure {
		return run, nil
	}

	fmt.Fprintln(out, cs.Bold("JOBS"))

	fmt.Fprintln(out, shared.RenderJobs(cs, jobs, true))

	if len(annotations) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, cs.Bold("ANNOTATIONS"))
		fmt.Fprintln(out, shared.RenderAnnotations(cs, annotations))
	}

	return run, nil
}
