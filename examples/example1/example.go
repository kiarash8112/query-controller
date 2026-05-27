package main

type GormDB struct{}

func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB  { return db }
func (db *GormDB) Create(value interface{}) *GormDB                     { return db }

func main() {
	users := []string{"admin", "guest"}
	db := &GormDB{}

	// Scenario 1: Fetch N+1 (TRUE POSITIVE)
	for _, u := range users {
		test1(db, u)
	}

	// Scenario 2: Transitive Fetch N+1 (TRUE POSITIVE)
	// for _, u := range users {
	// 	fetchTarget(u)
	// }

	// // Scenario 3: Transitive False Positive (STATIC FETCH)
	// for _, _ = range users {
	// 	fetchTarget("SELECT * FROM static_table")
	// }

	// // Scenario 4: Transitive Insert (IGNORED! Not a fetch)
	// for _, u := range users {
	// 	insertTarget(u)
	// }

	// // Scenario 5: Direct Insert (IGNORED! Not a fetch)
	// for _, u := range users {
	// 	db.Create(u)
	// }

	// DynamicBuildExample()
	// example3()
	// example4()
}

func test1(db *GormDB, u string) {
	test2(db, u)
}
func test2(db *GormDB, u string) {
	db.Where(u).Find(nil)
}
