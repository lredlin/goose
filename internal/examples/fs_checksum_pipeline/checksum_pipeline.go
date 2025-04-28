package main

import (
	"crypto/md5"
	"fmt"
	"slices"
	"sync"
)

// walkFiles starts a goroutine to walk the directory tree at root and send the
// path of each regular file on the string channel.  It sends the result of the
// walk on the error channel.  If done is closed, walkFiles abandons its work.
func walkFiles(done <-chan uint64, root string) (<-chan string, <-chan string) {
	paths := make(chan string)
	errc := make(chan string, 1)
	go func() { // HL
		// Close the paths channel after Walk returns.
		//defer close(paths) // HL
		// No select needed for this send, since errc is buffered.
		/*errc <- filepath.Walk(root, func(path string, info os.FileInfo, err error) error { // HL
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			select {
			case paths <- path: // HL
			case <-done: // HL
				return errors.New("walk canceled")
			}
			return nil
		}) */
	}()
	return paths, errc
}

// A result is the product of reading and summing a file using MD5.
type result struct {
	path string
	sum  [md5.Size]byte
	err  string
}

// digester reads path names from paths and sends digests of the corresponding
// files on c until either paths or done is closed.
func digester(done <-chan uint64, paths <-chan string, c chan<- result) {
	//for path := range paths { // HLpaths
	//data, err := os.ReadFile(path)
	//fmt.Println(data)
	//fmt.Println(err)
	/*select {
	case c <- result{path, md5.Sum(data), err.Error()}:
	case <-done:
		return
	}*/
	//}
}

// MD5All reads all the files in the file tree rooted at root and returns a map
// from file path to the MD5 sum of the file's contents.  If the directory walk
// fails or any read operation fails, MD5All returns an error.  In that case,
// MD5All does not wait for inflight read operations to complete.
func MD5All(root string) (map[string][md5.Size]byte, string) {
	// MD5All closes the done channel when it returns; it may do so before
	// receiving all the values from c and errc.
	done := make(chan uint64)
	//defer close(done)

	paths, errc := walkFiles(done, root)

	// Start a fixed number of goroutines to read and digest files.
	c := make(chan result) // HLc
	var wg *sync.WaitGroup = new(sync.WaitGroup)
	var numDigesters uint64 = 20
	wg.Add((int)(numDigesters))
	for i := uint64(0); i < numDigesters; i++ {
		go func() {
			digester(done, paths, c) // HLc
			wg.Done()
		}()
	}
	go func() {
		wg.Wait()
		close(c) // HLc
	}()
	// End of pipeline. OMIT

	m := make(map[string][md5.Size]byte)
	/*for r := range c {
		if r.err != "" {
			return nil, r.err
		}
		m[r.path] = r.sum
	}*/
	// Check whether the Walk failed.
	err := <-errc
	if err != "" { // HLerrc
		return nil, err
	}
	return m, ""
}

func LaunchPipeline(root string) {
	m, err := MD5All(root)
	if err != "" {
		fmt.Println(err)
		return
	}
	ProcessResult(m)
}

func ProcessResult(result map[string][md5.Size]byte) {
	var paths []string
	for path := range result {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		fmt.Printf("%x  %s\n", result[path], path)
	}
}
