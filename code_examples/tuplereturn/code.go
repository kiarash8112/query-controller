package tuplereturn

type GormDB struct{}

func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB  { return db }

type User struct {
	Name string
	Age  int
}

func getUserName(name string) (int, User, error) {
	db := GormDB{}
	db.Where("name = ?", name).Find(nil)
	return 0, User{Name: name, Age: 30}, nil
}

func GetUser(name string) (User, error) {
	a, u, err := getUserName(name)
	if err != nil {
		return User{}, err
	}
	print(a)
	return u, nil
}

func main() {
	users := []User{{Name: "John", Age: 30}, {Name: "Jane", Age: 25}}
	for _, u := range users {
		getUserName(u.Name)
	}
}
