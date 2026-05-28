package main

type GormDB struct{}

func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB  { return db }
func (db *GormDB) Create(value interface{}) *GormDB                     { return db }

func main() {
	users := []string{"admin", "guest"}
	db := &GormDB{}

	for _, u := range users {
		db.Where(u).Find(nil)
	}

}
