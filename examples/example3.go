package main

import (
	"context"
)

// -----------------------------------------------------
// 1. MOCKING GORM INTERFACES
// -----------------------------------------------------

func (db *DB) Group(query string) *DB                     { return db }
func (db *DB) Exec(sql string, values ...interface{}) *DB { return db }

type PartRequestState string

// -----------------------------------------------------
// 2. YOUR EXACT FUNCTION
// -----------------------------------------------------

func deleteItemPriceVoucherItem1(ctx context.Context, tx *DB, partRequestIDs []int64, userID int64) error {
	prTargetStateMap := make(map[int64]PartRequestState)

	for _, prID := range partRequestIDs {
		var targetState PartRequestState

		// QUERY 1: FETCH (True N+1)
		err := tx.Table("logistics.inv_part_request_item pri").
			Where("pri.part_request_id = ?", prID).
			Group("pri.part_request_id").
			Select("MIN(pri.state_c)").
			Scan(&targetState).Error()
		if err != nil {
			return err
		}

		// QUERY 2: UPDATE (Should be ignored since it's not a Fetch)
		err = tx.Exec(
			"UPDATE logistics.inv_part_request SET state_c = ? WHERE pr.id = ?",
			targetState, prID,
		).Error()
		if err != nil {
			return err
		}

		prTargetStateMap[prID] = targetState
	}

	return nil
}

// -----------------------------------------------------
// 3. MAIN TRIGGER
// -----------------------------------------------------
func example4() {
	tx := &DB{}
	ctx := context.Background()
	partRequestIDs := []int64{1, 2, 3}

	deleteItemPriceVoucherItem1(ctx, tx, partRequestIDs, 99)
}
