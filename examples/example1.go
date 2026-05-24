package main

// A custom builder function (like your FilterPriceType)
func customFilter(db *GormDB, id string) *GormDB {
	return db.Where("custom = ?", id)
}

func applyPathFilter(db *GormDB, paths []string) *GormDB {

	if paths != nil {
		for _, path := range paths {
			if path == "PRICE_KEY" {
				// The custom function call that breaks the analyzer!
				db = customFilter(db, path)
			} else {
				// Standard builder
				db = db.Where("part_id in (?)", path)
			}
		}
	}
	return db
}

func DynamicBuildExample() {
	db := &GormDB{}
	paths := []string{"1", "2"}

	// Phase 1: Build the query dynamically (No execution yet)
	db = applyPathFilter(db, paths)

	// Phase 2: Execute ONE time outside the loop
	db.Find(nil)
}
