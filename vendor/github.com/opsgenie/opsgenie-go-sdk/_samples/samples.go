/*
Copyright 2015 OpsGenie. All rights reserved.
Use of this source code is governed by a Apache Software
license that can be found in the LICENSE file.
*/

//Package samples provides common utility functions used in samples.
package samples

import (
	"crypto/rand"
)

// RandString function is used for generating randomly generated strings initialized with a given prefix
// with the length of given number.
// This function is called when creating a new Alert.
func RandString(n int) string {
	const alphanum = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var bytes = make([]byte, n)
	rand.Read(bytes)
	for i, b := range bytes {
		bytes[i] = alphanum[b%byte(len(alphanum))]
	}
	return string(bytes)
}

// RandStringWithPrefix adds a prefix to the string generated by RandString method.
func RandStringWithPrefix(prefix string, n int) string {
	return prefix + "-" + RandString(n)
}
