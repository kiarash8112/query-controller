package main

import (
	"fmt"

	db "github.com/kiarash8112/querycontroller/examples"
	"github.com/kiarash8112/querycontroller/examples/example2"
)

type User struct {
	name string
}

func main() {
	users := []User{User{name: "admin"}, User{name: "guest"}}
	db := &db.GormDB{}

	for _, u := range users {
		
		pritn()

		example2.GetUser(db, u.name)
	}

}

func pritn() {
	example2.DoSome()
	fmt.Println("got here")
}
