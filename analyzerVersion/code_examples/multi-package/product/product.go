package product

type GormDB struct{}

func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB  { return db }

func FetchProduct(id int) {
	db := GormDB{}
	db.Where("id = ?", id).Find(nil)
}
