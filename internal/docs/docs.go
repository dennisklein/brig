// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package docs says where brig's documentation is published, so that help
// texts and hints can link to a page of it.
package docs

// Base is the URL of the docs directory. It is the one place to change if
// the docs move, such as to a website.
const Base = "https://github.com/dennisklein/brig/blob/main/docs/"

// URL returns the URL of page, a path under docs/ with an optional
// #heading, such as troubleshooting.md#host.
func URL(page string) string { return Base + page }
