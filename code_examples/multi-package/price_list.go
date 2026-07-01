package multipackage

import "github.com/kiarash8112/query-controller/code_examples/multi-package/product"

func GetPriceList(ids []int) {
	for _, id := range ids {
		product.FetchProduct(id)
	}
}
