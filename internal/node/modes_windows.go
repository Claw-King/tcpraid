package node

// modesMeanAccess: on Windows the permission bits Go reports do not say who can get at
// a file (access is in the file's access control list), so they are not checked.
const modesMeanAccess = false
