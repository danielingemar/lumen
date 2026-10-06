# Trusted signing keys

Every file here named `<key id>.pub` holds one **public** Ed25519 key (base64 of the 32 bytes). A licence is accepted
only if it is signed by one of these keys. The keys are built into the program: nothing is read from the network, and
nothing outside the build can add a key.

This directory is empty in the open-source repository, so no licence can be verified and the program runs as the
Community edition. The publisher of a commercial build creates a key pair with `lumen-license keygen`, commits the
`.pub` file here and keeps the private key secret (see docs/LICENSING.md).
