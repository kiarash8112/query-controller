package basescenario

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

	for _, u := range users {
		GetUser(db, u.name)
	}

}

func GetUser(db *GormDB, u string) {
	db.Where("it is", u).Find(nil)
}
