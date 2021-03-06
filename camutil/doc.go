/*
Copyright 2013 Tamás Gulácsi

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
Package camutil copies some unexported utilities from camlistore.org/cmd/cam{get,put}
*/
package camutil

// Verbose shall be true for verbose HTTP logging
var Verbose = false

// InsecureTLS sets client's InsecureTLS
var InsecureTLS bool

// SkipIrregular makes camget skip not regular files.
var SkipIrregular bool
