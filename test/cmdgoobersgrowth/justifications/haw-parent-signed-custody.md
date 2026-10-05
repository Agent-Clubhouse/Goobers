# HAW-CHD-003 contained parent custody identity

The command blob bridge explicitly refuses the new signed parent-pod identity
until its separate parent authority adapter is installed. Signing and MAC-domain
verification stay in internal/podauth. This one guard prevents future contained
parents from inheriting ordinary shared blob access while their dedicated
custody path is assembled.
