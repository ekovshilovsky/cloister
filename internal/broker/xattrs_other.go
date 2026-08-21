// Proprietary and confidential. All rights reserved.

//go:build !darwin

package broker

func listXattrs(string) ([]string, error) {
	return nil, nil
}
