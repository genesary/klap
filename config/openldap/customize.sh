#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

# Load libraries
. /opt/bitnami/scripts/libos.sh
. /opt/bitnami/scripts/libopenldap.sh

# Load LDAP environment variables
eval "$(ldap_env)"

cat >> "${LDAP_SHARE_DIR}/custom.ldif" << EOF
dn: olcDatabase={2}mdb,cn=config
changetype: modify
add: olcAccess
olcAccess: {0}to *
    by dn.base="gidNumber=0+uidNumber=${UID},cn=peercred,cn=external,cn=auth" manage
    by * none break

EOF

ldap_start_bg
debug_execute ldapmodify -Y EXTERNAL -H "ldapi:///" -f "${LDAP_SHARE_DIR}/custom.ldif"
ldap_stop
