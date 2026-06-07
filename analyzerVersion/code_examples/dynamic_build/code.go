package dynamicbuild

type GormDB struct{}

func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB  { return db }
func (db *GormDB) Create(value interface{}) *GormDB                     { return db }

type User struct {
	name string
}

func main() {
	users := []User{{name: "admin"}, {name: "guest"}}
	db := &GormDB{}

	query := &GormDB{}

	for _, u := range users {
		query = createWhere(db, u.name)
	}

	query.Find(nil)
}

func createWhere(db *GormDB, u string) *GormDB {
	return db.Where("it is", u)
}
