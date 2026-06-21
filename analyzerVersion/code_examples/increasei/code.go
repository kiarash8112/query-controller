package increasei

type GormDB struct{}

func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB  { return db }

type User struct {
	name string
}

func main() {
	users := []User{{name: "admin"}, {name: "guest"}}
	db := &GormDB{}
	n := len(users)

	for i := 0; i < n; i++ {
		query(db, users, i)
	}
}

func query(db *GormDB, users []User, i int) {
	db.Where("id = ?", users[i].name).Find(nil)
}
