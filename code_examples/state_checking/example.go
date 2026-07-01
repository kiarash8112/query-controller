package statechecking

type GormDB struct{}

func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB  { return db }

func main() {
	var a string
	db := &GormDB{}

	for {
		if condition() {
			query(db, a)
		}
	}
}

func condition() bool {
	return true
}

func query(db *GormDB, a string) {
	db.Where("id = ?", a).Find(nil)
}
