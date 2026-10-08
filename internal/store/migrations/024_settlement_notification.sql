CREATE UNIQUE INDEX driver_notifications_settled_run_idx
ON driver_notifications(attempt_id, worker_run_generation)
WHERE kind='settled';
