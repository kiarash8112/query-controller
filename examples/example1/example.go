package main

import (
	db "github.com/kiarash8112/querycontroller/examples"
)

type User struct {
	name string
}

func main() {
	users := []User{{name: "admin"}, {name: "guest"}}
	db := &db.GormDB{}

	for _, u := range users {
		GetUser(db, u.name)
	}

}

func GetUser(db *db.GormDB, u string) {
	db.Where("it is", u).Find(nil)
}

func getID() int {
	return getID() // Recursive call
}

func main1(db *db.GormDB) {
	id := getID()
	db.Where("id = ?", id) // Sink
}
