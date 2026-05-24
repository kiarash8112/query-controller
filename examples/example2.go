package main

import "context"

// -----------------------------------------------------
// 1. MOCKING GORM INTERFACES
// -----------------------------------------------------

type DB struct{}

func (db *DB) Raw(sql string, values ...interface{}) *DB         { return db }
func (db *DB) Scan(dest interface{}) *DB                         { return db }
func (db *DB) Table(name string) *DB                             { return db }
func (db *DB) Where(query interface{}, args ...interface{}) *DB  { return db }
func (db *DB) Select(query interface{}, args ...interface{}) *DB { return db }
func (db *DB) Error() error                                      { return nil }

// 1b. Mocking your external 'iv' object
type IV struct{}

func (i *IV) UpdateStatesOnVoucherDeleteTX(ctx context.Context, db *DB, itemIDs []int64) error {
	// Let's assume this executes another query under the hood
	db.Where("item_id IN ?", itemIDs).Scan(nil)
	return nil
}

var iv = &IV{}

// -----------------------------------------------------
// 2. YOUR TARGET FUNCTION
// -----------------------------------------------------

func deleteItemPriceVoucherItem(ctx context.Context, db *DB, finVoucherItemIDs []int64) error {
	itemPriceIDs := make([]int64, 0)
	itemIDs := make([]int64, 0)

	// QUERY 1: RAW + SCAN
	err := db.Raw(
		"delete from logistics.inv_item_price_voucher_item ipvi where voucher_item_id in ? RETURNING ipvi.item_price_id",
		finVoucherItemIDs,
	).Scan(&itemPriceIDs).Error()
	if err != nil {
		return err
	}

	// QUERY 2: CHAINED BUILDER + SCAN
	err = db.Table("inv_voucher_item_price").
		Where("id IN ?", itemPriceIDs).
		Select("inventory_voucher_item_id").
		Scan(&itemIDs).Error()
	if err != nil {
		return err
	}

	// QUERY 3: TRANSITIVE EXECUTOR
	err = iv.UpdateStatesOnVoucherDeleteTX(ctx, db, itemIDs)

	return err
}

// -----------------------------------------------------
// 3. THE ENTRY POINT (To trigger the N+1 scanner)
// -----------------------------------------------------

func example3() {
	db := &DB{}
	ctx := context.Background()

	batchesOfIDs := [][]int64{{1, 2}, {3, 4}, {5, 6}}

	// N+1 SCENARIO: Calling your complex deletion flow inside a loop!
	for _, batch := range batchesOfIDs {
		deleteItemPriceVoucherItem(ctx, db, batch)
	}
}
