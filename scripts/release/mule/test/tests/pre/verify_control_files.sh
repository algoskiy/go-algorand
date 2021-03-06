#!/usr/bin/env bash
# shellcheck disable=2035,2076

set -ex

echo "[$0] Testing control files"

RPMTMP=$(mktemp -d)

if [ "$PKG_TYPE" = deb ]
then
    #
    # We're looking for a line that looks like the following:
    #
    #       Pre-Depends: algorand (>= 2.1.6)
    #

    cp "./tmp/node_pkgs/$OS_TYPE/$ARCH_TYPE"/algorand-devtools*"$CHANNEL"*"$VERSION"*.deb "$RPMTMP"
    cd "$RPMTMP"
    ar xv *"$VERSION"*.deb
    tar xf control.tar.xz

    if ! grep -F "Pre-Depends: $ALGORAND_PACKAGE_NAME (>= $VERSION)" control
    then
        echo "[$0] The dependency for $ALGORAND_PACKAGE_NAME version $VERSION is incorrect."
        exit 1
    fi

    echo "[$0] The dependency for $ALGORAND_PACKAGE_NAME version $VERSION is correct."
else
    # Note that the .spec file isn't packaged in the RPM. There are tools such `rpmrebuild` that
    # attempt to generate the .spec file, but it doesn't give us the info we need.
    #
    # Instead, we'll just install using `dpkg` and grep the error stream.
    #
    # Also, note that the RPM package DOES NOT have the CHANNEL in its filename (unlike DEB)!!
    if ! rpm -i "./tmp/node_pkgs/$OS_TYPE/$ARCH_TYPE/algorand-devtools-"*"$VERSION"*"$ARCH_BIT".rpm 2> "$RPMTMP/rpm.install"
    then
        #
        # We're looking for lines that looks like the following:
        #
        #       error: Failed dependencies:
        #               algorand >= 2.1.86017 is needed by algorand-devtools-2.1.86017-1.x86_64
        #
        if [[ $(cat "$RPMTMP/rpm.install") =~ "$ALGORAND_PACKAGE_NAME >= $VERSION is needed by $DEVTOOLS_PACKAGE_NAME-$VERSION" ]]
        then
            echo "[$0] The package \`algorand-devtools\` correctly has a dependency on package $ALGORAND_PACKAGE_NAME and failed to install."
            exit 0
        fi

        echo "[$0] The package \`algorand-devtools\` failed to install because of a missing dependency other than the $ALGORAND_PACKAGE_NAME package."
        exit 1
    else
        echo "[$0] The package \`algorand-devtools\` was installed without any dependencies, while it should have a dependency on the $ALGORAND_PACKAGE_NAME package."
        exit 1
    fi
fi

