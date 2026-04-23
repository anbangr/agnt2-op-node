// SPDX-License-Identifier: MIT
pragma solidity 0.8.15;

// Testing utilities
import { CommonTest } from "test/setup/CommonTest.sol";

// Libraries
import { Constants } from "src/libraries/Constants.sol";
import { Hashing } from "src/libraries/Hashing.sol";
import { Encoding } from "src/libraries/Encoding.sol";
import { Bytes32AddressLib } from "@rari-capital/solmate/src/utils/Bytes32AddressLib.sol";

// Target contract dependencies
import { AddressAliasHelper } from "src/vendor/AddressAliasHelper.sol";

// Target contract
import { CrossDomainOwnable3 } from "src/L2/CrossDomainOwnable3.sol";

/// @title CrossDomainOwnable3_XDomainSetter_Harness
/// @notice Harness that extends `CrossDomainOwnable3` with an owner-gated setter so that the
///         access-control behavior of `_checkOwner` can be exercised end-to-end.
contract CrossDomainOwnable3_XDomainSetter_Harness is CrossDomainOwnable3 {
    uint256 public value;

    function set(uint256 _value) external onlyOwner {
        value = _value;
    }
}

/// @title CrossDomainOwnable3_TestInit
/// @notice Reusable test initialization for `CrossDomainOwnable3` tests.
abstract contract CrossDomainOwnable3_TestInit is CommonTest {
    CrossDomainOwnable3_XDomainSetter_Harness setter;

    /// @notice CrossDomainOwnable3.sol transferOwnership event
    event OwnershipTransferred(address indexed previousOwner, address indexed newOwner, bool isLocal);

    function setUp() public virtual override {
        super.setUp();
        vm.prank(alice);
        setter = new CrossDomainOwnable3_XDomainSetter_Harness();
    }
}

/// @title CrossDomainOwnable3_Constructor_Test
/// @notice Tests the initial state of a freshly deployed `CrossDomainOwnable3`.
contract CrossDomainOwnable3_Constructor_Test is CrossDomainOwnable3_TestInit {
    /// @notice Tests that the deployer becomes the owner and `isLocal` defaults to true.
    function test_constructor_succeeds() public view {
        assertEq(setter.owner(), alice);
        assertEq(setter.isLocal(), true);
    }
}

/// @title CrossDomainOwnable3_TransferOwnership_Test
/// @notice Tests for the ownership transfer functionality of `CrossDomainOwnable3`.
contract CrossDomainOwnable3_TransferOwnership_Test is CrossDomainOwnable3_TestInit {
    /// @notice Tests that `transferOwnership(address,bool)` reverts when the caller is not the
    ///         owner across arbitrary non-owner callers, new owners, and locality flags.
    function testFuzz_transferOwnership_notOwner_reverts(address _caller, address _newOwner, bool _isLocal) public {
        vm.assume(_caller != setter.owner());
        vm.prank(_caller);
        vm.expectRevert("CrossDomainOwnable3: caller is not the owner");
        setter.transferOwnership({ _owner: _newOwner, _isLocal: _isLocal });
    }

    /// @notice Tests that `transferOwnership(address,bool)` reverts when transferring to the zero
    ///         address for any locality flag.
    function testFuzz_transferOwnership_zeroAddress_reverts(bool _isLocal) public {
        vm.prank(setter.owner());
        vm.expectRevert("CrossDomainOwnable3: new owner is the zero address");
        setter.transferOwnership({ _owner: address(0), _isLocal: _isLocal });
    }

    /// @notice Tests that the inherited `transferOwnership(address)` reverts for the zero address.
    function test_transferOwnership_noLocalZeroAddress_reverts() public {
        vm.prank(setter.owner());
        vm.expectRevert("Ownable: new owner is the zero address");
        setter.transferOwnership(address(0));
    }

    /// @notice Tests that `transferOwnership(address,bool)` succeeds when the caller is the owner
    ///         and locality is kept local for a range of new owners and stored values.
    function testFuzz_localTransferOwnership_succeeds(address _newOwner, uint256 _value) public {
        vm.assume(_newOwner != address(0));

        vm.expectEmit(true, true, true, true, address(setter));
        emit OwnershipTransferred(alice, _newOwner);
        emit OwnershipTransferred(alice, _newOwner, true);

        vm.prank(setter.owner());
        setter.transferOwnership({ _owner: _newOwner, _isLocal: true });

        assertEq(setter.isLocal(), true);
        assertEq(setter.owner(), _newOwner);

        vm.prank(_newOwner);
        setter.set(_value);
        assertEq(setter.value(), _value);
    }

    /// @notice The existing transferOwnership(address) method still exists on the contract.
    function test_transferOwnershipNoLocal_succeeds() public {
        bool isLocal = setter.isLocal();

        vm.expectEmit(true, true, true, true, address(setter));
        emit OwnershipTransferred(alice, bob);

        vm.prank(setter.owner());
        setter.transferOwnership(bob);

        // isLocal has not changed
        assertEq(setter.isLocal(), isLocal);

        vm.prank(bob);
        setter.set(2);
        assertEq(setter.value(), 2);
    }

    /// @notice Tests that `transferOwnership(address,bool)` succeeds when transferring to a
    ///         cross-domain owner and that a relayed message from that owner can invoke the
    ///         owner-only setter.
    function testFuzz_crossDomainTransferOwnership_succeeds(address _newOwner, uint256 _value) public {
        vm.assume(_newOwner != address(0));
        vm.assume(_newOwner != address(l2CrossDomainMessenger));
        // The messenger treats DEFAULT_L2_SENDER as "unset" and reverts when it is surfaced via
        // `xDomainMessageSender()`, which would fail the onlyOwner check inside the relayed call.
        vm.assume(_newOwner != Constants.DEFAULT_L2_SENDER);

        vm.expectEmit(true, true, true, true, address(setter));
        emit OwnershipTransferred(alice, _newOwner);
        emit OwnershipTransferred(alice, _newOwner, false);

        vm.prank(setter.owner());
        setter.transferOwnership({ _owner: _newOwner, _isLocal: false });

        assertEq(setter.isLocal(), false);
        assertEq(setter.owner(), _newOwner);

        // Simulate the L2 execution where the call is coming from the L1CrossDomainMessenger
        vm.prank(AddressAliasHelper.applyL1ToL2Alias(address(l1CrossDomainMessenger)));
        l2CrossDomainMessenger.relayMessage(
            Encoding.encodeVersionedNonce(1, 1),
            _newOwner,
            address(setter),
            0,
            0,
            abi.encodeCall(CrossDomainOwnable3_XDomainSetter_Harness.set, (_value))
        );

        assertEq(setter.value(), _value);
    }
}

/// @title CrossDomainOwnable3_CheckOwner_Test
/// @notice Tests for the overridden `_checkOwner` function, exercised through the harness's
///         `onlyOwner`-gated `set` function in both local and cross-domain modes.
contract CrossDomainOwnable3_CheckOwner_Test is CrossDomainOwnable3_TestInit {
    /// @notice Tests that `_checkOwner` reverts in local mode when the caller is not the owner.
    function testFuzz_checkOwner_localNotOwner_reverts(address _caller, uint256 _value) public {
        vm.assume(_caller != setter.owner());
        vm.prank(_caller);
        vm.expectRevert("CrossDomainOwnable3: caller is not the owner");
        setter.set(_value);
    }

    /// @notice Tests that `_checkOwner` allows the owner to call `onlyOwner` in local mode.
    function testFuzz_checkOwner_localOwner_succeeds(uint256 _value) public {
        assertEq(setter.isLocal(), true);
        vm.prank(setter.owner());
        setter.set(_value);
        assertEq(setter.value(), _value);
    }

    /// @notice Tests that `_checkOwner` reverts in cross-domain mode when the messenger's
    ///         `xDomainMessageSender` does not match the owner, even though the caller is the
    ///         messenger itself.
    function testFuzz_checkOwner_crossDomainNotOwner_reverts(address _xDomainSender) public {
        vm.assume(_xDomainSender != alice);
        // `xDomainMessageSender()` reverts when the stored value equals DEFAULT_L2_SENDER, so
        // that case cannot exercise the owner-mismatch revert this test targets.
        vm.assume(_xDomainSender != Constants.DEFAULT_L2_SENDER);

        vm.expectEmit(true, true, true, true);
        // OpenZeppelin Ownable.sol transferOwnership event
        emit OwnershipTransferred(alice, alice);
        // CrossDomainOwnable3.sol transferOwnership event
        emit OwnershipTransferred(alice, alice, false);

        vm.prank(setter.owner());
        setter.transferOwnership({ _owner: alice, _isLocal: false });

        // set the xDomainMsgSender storage slot
        bytes32 key = bytes32(uint256(204));
        bytes32 value = Bytes32AddressLib.fillLast12Bytes(_xDomainSender);
        vm.store(address(l2CrossDomainMessenger), key, value);

        vm.prank(address(l2CrossDomainMessenger));
        vm.expectRevert("CrossDomainOwnable3: caller is not the owner");
        setter.set(1);
    }

    /// @notice Tests that a relayed message to the setter fails (surfaced via a
    ///         `FailedRelayedMessage` event) when the cross-domain sender is not the owner.
    function test_checkOwner_crossDomainRelayNotOwner_reverts() public {
        vm.expectEmit(true, true, true, true);
        // OpenZeppelin Ownable.sol transferOwnership event
        emit OwnershipTransferred(alice, alice);
        // CrossDomainOwnable3.sol transferOwnership event
        emit OwnershipTransferred(alice, alice, false);

        vm.prank(setter.owner());
        setter.transferOwnership({ _owner: alice, _isLocal: false });

        assertEq(setter.isLocal(), false);

        uint240 nonce = 0;
        address sender = bob;
        address target = address(setter);
        uint256 value = 0;
        uint256 minGasLimit = 0;
        bytes memory message = abi.encodeCall(CrossDomainOwnable3_XDomainSetter_Harness.set, (1));

        bytes32 hash = Hashing.hashCrossDomainMessage(
            Encoding.encodeVersionedNonce(nonce, 1), sender, target, value, minGasLimit, message
        );

        // It should be a failed message. The revert is caught, so we cannot expectRevert here.
        vm.expectEmit(true, true, true, true, address(l2CrossDomainMessenger));
        emit FailedRelayedMessage(hash);

        vm.prank(AddressAliasHelper.applyL1ToL2Alias(address(l1CrossDomainMessenger)));
        l2CrossDomainMessenger.relayMessage(
            Encoding.encodeVersionedNonce(nonce, 1), sender, target, value, minGasLimit, message
        );

        assertEq(setter.value(), 0);
    }

    /// @notice Tests that `_checkOwner` reverts in cross-domain mode when the caller is not the
    ///         messenger.
    function testFuzz_checkOwner_crossDomainNotMessenger_reverts(address _caller) public {
        vm.assume(_caller != address(l2CrossDomainMessenger));

        vm.expectEmit(true, true, true, true);
        // OpenZeppelin Ownable.sol transferOwnership event
        emit OwnershipTransferred(alice, alice);
        // CrossDomainOwnable3.sol transferOwnership event
        emit OwnershipTransferred(alice, alice, false);

        vm.prank(setter.owner());
        setter.transferOwnership({ _owner: alice, _isLocal: false });

        vm.prank(_caller);
        vm.expectRevert("CrossDomainOwnable3: caller is not the messenger");
        setter.set(1);
    }
}
