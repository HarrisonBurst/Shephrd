package store

import (
	"database/sql"
	"fmt"
)

const orderedPositionOffset = 1000000000

type orderedPositionSet struct {
	table       string
	ownerColumn string
	rowColumn   string
}

type orderedPositionOperation uint8

const (
	openOrderedPosition orderedPositionOperation = iota
	moveOrderedPosition
	normalizeOrderedPositions
)

type orderedPositionMutation struct {
	operation orderedPositionOperation
	rowID     string
	position  int
}

var (
	planItemPositions   = orderedPositionSet{table: "plan_items", ownerColumn: "plan_id", rowColumn: "id"}
	planReportPositions = orderedPositionSet{table: "plan_report_inputs", ownerColumn: "item_id", rowColumn: "prerequisite_item_id"}
)

func mutateOrderedPositionsTx(tx *sql.Tx, set orderedPositionSet, ownerID string, mutation orderedPositionMutation) error {
	rows, err := tx.Query(fmt.Sprintf(`SELECT %s FROM %s WHERE %s=? ORDER BY position`, set.rowColumn, set.table, set.ownerColumn), ownerID)
	if err != nil {
		return err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	positions := make([]int, len(ids))
	switch mutation.operation {
	case openOrderedPosition:
		if mutation.position < 1 || mutation.position > len(ids)+1 {
			return fmt.Errorf("position must be between 1 and %d", len(ids)+1)
		}
		for index := range ids {
			positions[index] = index + 1
			if positions[index] >= mutation.position {
				positions[index]++
			}
		}
	case moveOrderedPosition:
		remaining := make([]string, 0, len(ids))
		found := false
		for _, id := range ids {
			if id == mutation.rowID {
				found = true
				continue
			}
			remaining = append(remaining, id)
		}
		if !found {
			return fmt.Errorf("ordered row %s does not exist", mutation.rowID)
		}
		if mutation.position < 1 || mutation.position > len(ids) {
			return fmt.Errorf("position must be between 1 and %d", len(ids))
		}
		ids = append(remaining, "")
		copy(ids[mutation.position:], ids[mutation.position-1:])
		ids[mutation.position-1] = mutation.rowID
		for index := range ids {
			positions[index] = index + 1
		}
	case normalizeOrderedPositions:
		for index := range ids {
			positions[index] = index + 1
		}
	default:
		return fmt.Errorf("unknown ordered position mutation %d", mutation.operation)
	}

	for index, id := range ids {
		if _, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET position=? WHERE %s=? AND %s=?`, set.table, set.ownerColumn, set.rowColumn), orderedPositionOffset+index, ownerID, id); err != nil {
			return err
		}
	}
	for index, id := range ids {
		if _, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET position=? WHERE %s=? AND %s=?`, set.table, set.ownerColumn, set.rowColumn), positions[index], ownerID, id); err != nil {
			return err
		}
	}
	return nil
}
