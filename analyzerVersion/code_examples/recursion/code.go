package recursion

type GormDB struct{}

func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB  { return db }

// type User struct {
// 	name string
// }

// func a(u string) {
// 	b(u)
// 	db := &GormDB{}
// 	db.Where("id = ?", u).Find(nil)
// }

// func b(u string) {
// 	a(u)
// 	db := &GormDB{}
// 	db.Where("id = ?", u).Find(nil)

// }

// func FetchForm(db *GormDB, u string) (int, string) {

// 	db.Where("id = ?", u).Find(nil)
// 	FetchForm(db, u)
// 	return 1, "a"
// }

// func main() {
// 	users := []User{{name: "admin"}, {name: "guest"}}
// 	db := &GormDB{}

// 	for _, u := range users {
// 		FetchForm(db, u.name)
// 	}
// }

var db = &GormDB{}

func sinkDB(q string) {
	db.Where("id = ?", q) // q is sink param 0
}
func middle(a, b string) {
	sinkDB(a) // propagates sink → middle param 0
	sinkDB(b) // propagates sink → middle param 1
}
