package nestedfunction

type GormDB struct{}

func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB  { return db }

type User struct {
	name string
}

func main() {
	users := []User{{name: "admin"}, {name: "guest"}}
	db := &GormDB{}

	for _, u := range users {
		depth1RunQuery(db, u.name)
	}
}

func depth1RunQuery(db *GormDB, name string) {
	runQuery(db, name)
}

func runQuery(db *GormDB, name string) {
	query(db, name)
}

func query(db *GormDB, u string) {
	db.Where("id = ?", u).Find(nil)
}
