package jwcc_test

import (
	"fmt"

	"github.com/djylb/cfgfile/jwcc"
)

func ExampleValue_Set() {
	doc, err := jwcc.Parse([]byte("{\n  // where to listen\n  \"listen\": \":80\",\n}\n"), nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	_ = doc.SetValue(jwcc.Path{"listen"}, ":8080")
	_ = doc.Set(jwcc.Path{"log", "level"}, []byte(`"debug"`))
	fmt.Print(string(doc.Pack()))
	// Output:
	// {
	//   // where to listen
	//   "listen": ":8080",
	//   "log": {
	//     "level": "debug"
	//   },
	// }
}

func ExampleFormat() {
	doc, _ := jwcc.Parse([]byte(`{"a":1, // one
"b":[1,2]}`), nil)
	fmt.Print(string(jwcc.Format(doc, nil)))
	// Output:
	// {
	//   "a": 1, // one
	//   "b": [1, 2],
	// }
}
