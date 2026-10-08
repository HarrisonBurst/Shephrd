package cli

import (
	"context"
	"encoding/json"
	"errors"
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
	"shephrd/internal/store"
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
ack command after its handling turn settles. A rejected or undeliverable
delivery stops renewal so the claim expires after wake.claim_ttl; after
wake_watch.max_rejected_claims consecutive such claims the notification is
parked for this watcher until wake unpark. While the watcher runs, wake drain
and wake pump for the same owner fail closed. SIGINT or SIGTERM stops it
without acknowledging or releasing claims.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			app := get()
			if !app.config.Wake.Enabled {
				return fmt.Errorf("wake watch is disabled by wake.enabled")
			}
			owner, err := watchOwner(driverID)
			if err != nil {
				return err
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
				MaxRejectedClaims: app.config.WakeWatch.MaxRejectedClaims,
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

func watchOwner(driverID string) (string, error) {
	owner := strings.TrimSpace(driverID)
	if owner == "" {
		return "", domain.Failure("driver_context_required", "--driver-id is required for wake watch")
	}
	if !wakewatch.Eligible(owner) {
		return "", domain.Failure("wake_watch_owner_refused", "wake watch serves non-Pi main drivers only; %s belongs to the Pi watcher or a sub-driver", owner)
	}
	return owner, nil
}

func parkedCommand(get func() *application) *cobra.Command {
	var driverID string
	command := &cobra.Command{
		Use:   "parked",
		Short: "List notifications wake watch parked after repeated rejected or undeliverable claims",
		Long: `List pending notifications that wake watch skips for one owner because
their last wake_watch.max_rejected_claims consecutive watcher claims ended
rejected or undeliverable. This is read-only, safe while the watcher runs, and
never prints claim tokens. Ordinary wake drain still claims parked notifications.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			app := get()
			owner, err := watchOwner(driverID)
			if err != nil {
				return err
			}
			parked, err := app.store.ParkedNotifications(owner, app.config.WakeWatch.MaxRejectedClaims)
			if err != nil {
				return err
			}
			result := map[string]any{"driver_id": owner, "max_rejected_claims": app.config.WakeWatch.MaxRejectedClaims, "parked": parked}
			return app.print(result, func() {
				for _, notice := range parked {
					fmt.Fprintf(app.out, "%s\t%s\t%d\t%s\t%s\n", notice.NotificationID, notice.Kind, notice.RejectedClaims, notice.LastResult, notice.LastDetail)
				}
			})
		},
	}
	command.Flags().StringVar(&driverID, "driver-id", "", "Non-Pi main driver owner served by wake watch")
	return command
}

func unparkCommand(get func() *application) *cobra.Command {
	var driverID string
	command := &cobra.Command{
		Use:   "unpark <notification-id>",
		Short: "Let wake watch deliver a parked notification again under a new claim",
		Long: `Record that the owner wants wake watch to deliver one parked notification
again. The next watcher drain claims it under a new claim token and webhook-id
in FIFO order. This is safe while the watcher runs; it does not claim,
acknowledge, answer, or change notification state.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := get()
			owner, err := watchOwner(driverID)
			if err != nil {
				return err
			}
			parked, err := app.store.UnparkNotification(owner, args[0], app.config.WakeWatch.MaxRejectedClaims)
			if errors.Is(err, store.ErrNotificationConflict) {
				return domain.Failure("notification_not_parked", "%v; list parked notifications with shephrd wake parked --driver-id %s --json", err, owner)
			}
			if err != nil {
				return err
			}
			result := struct {
				domain.ParkedNotification
				Unparked bool `json:"unparked"`
			}{parked, true}
			return app.print(result, func() {
				fmt.Fprintf(app.out, "unparked %s after %d rejected or undeliverable claims\n", parked.NotificationID, parked.RejectedClaims)
			})
		},
	}
	command.Flags().StringVar(&driverID, "driver-id", "", "Non-Pi main driver owner served by wake watch")
	return command
}
