Your own certificate goes here, installed by:

  python3 setup.py cert --cert fullchain.pem --key privkey.pem

It copies the files in as fullchain.pem and privkey.pem (the key readable
only by the Janus container user, uid 65532) and switches Janus from its
self-signed certificate. Run it again with renewed files; Janus reloads
them without a restart. `python3 setup.py cert --self-signed` switches back.
