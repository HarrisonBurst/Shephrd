package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"shephrd/internal/driverdelivery"
	domain "shephrd/internal/model"
	"shephrd/internal/wakewatch"
)

const wakeWatchDeliveryTimeout = 10 * time.Second

func watchCommand(get func() *application) *cobra.Command {
	var driverID string
	var jsonLog bool
	command := &cobra.Command{
		Use:   "watch",
		Short: "Deliver one main-driver notification claim at a time to the trusted delivery extension until stopped",
		Long: `Run a foreground, driver-agnostic notification watcher for one non-Pi main
driver owner. It claims one notification at a time, hands it to the pinned
driver.delivery extension configured in [wake_watch.delivery], renews the claim
up to wake_watch.renew_horizon, runs pump-only sub-driver activation passes
while the claim is outstanding, and detects settlement from stored notification
state. The watcher never acknowledges: the receiving driver runs the delivered
ack command after its handling turn settles. While the watcher runs, wake drain
and wake pump for the same owner fail closed. SIGINT or SIGTERM stops it
without acknowledging or releasing claims.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			app := get()
			if !app.config.Wake.Enabled {
				return fmt.Errorf("wake watch is disabled by wake.enabled")
			}
			owner := strings.TrimSpace(driverID)
			if owner == "" {
				return domain.Failure("driver_context_required", "--driver-id is required for wake watch")
			}
			if strings.HasPrefix(owner, "driver:pi:") || domain.IsSubdriverOwner(owner) {
				return domain.Failure("wake_watch_owner_refused", "wake watch serves non-Pi main drivers only; %s belongs to the Pi watcher or a sub-driver", owner)
			}
			delivery := app.config.WakeWatch.Delivery
			if delivery == nil {
				return domain.Failure("wake_watch_unconfigured", "wake watch requires a trusted [wake_watch.delivery] extension")
			}
			generation := "watch:" + uuid.NewString()
			lock, err := wakewatch.Acquire(app.config.DataDir, wakewatch.Holder{DriverID: owner, Generation: generation, PID: os.Getpid(), StartedAt: time.Now().UTC()})
			if err != nil {
				return err
			}
			defer lock.Close()
			deliverer := driverdelivery.NewExtensionDeliverer(*delivery, os.Environ())
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			describeContext, cancel := context.WithTimeout(ctx, wakeWatchDeliveryTimeout)
			err = deliverer.Describe(describeContext)
			cancel()
			if err != nil {
				return fmt.Errorf("wake watch delivery extension failed verification: %w", err)
			}
			encoder := json.NewEncoder(app.out)
			encoder.SetEscapeHTML(false)
			watcher := wakewatch.Watcher{
				Store: app.store, Activation: app.control, Deliverer: deliverer,
				DriverID: owner, Generation: generation, ClaimTTL: app.config.Wake.ClaimTTL,
				PollMin: app.config.WakeWatch.PollMin, PollMax: app.config.WakeWatch.PollMax,
				RenewHorizon: app.config.WakeWatch.RenewHorizon, DeliveryTimeout: wakeWatchDeliveryTimeout,
				Log: func(event wakewatch.Event) {
					if jsonLog || app.json {
						_ = encoder.Encode(event)
						return
					}
					fmt.Fprintf(app.out, "%s\t%s\t%s\t%s\n", event.Time.Format(time.RFC3339), event.Event, event.NotificationID, event.Detail)
				},
			}
			watcher.Run(ctx)
			return nil
		},
	}
	command.Flags().StringVar(&driverID, "driver-id", "", "Non-Pi main driver owner whose notifications this watcher delivers")
	command.Flags().BoolVar(&jsonLog, "json-log", false, "Write one JSON object per watcher event")
	return command
}
