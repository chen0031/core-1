let sidechainGateMS = artifacts.require('./MultiSigWallet.sol');
let Gatekeeper = artifacts.require('./SimpleGatekeeperWithLimit.sol');
let SNM = artifacts.require('./SNM.sol');

let MSOwners = [
    '0xdaec8F2cDf27aD3DF5438E5244aE206c5FcF7fCd',
    '0xd9a43e16e78c86cf7b525c305f8e72723e0fab5e',
    '0x72cb2a9AD34aa126fC02b7d32413725A1B478888',
    '0x1f50Be5cbFBFBF3aBD889e17cb77D31dA2Bd7227',
    '0xe062C67207F7E478a93EF9BEA39535d8EfFAE3cE',
    '0x5fa359a9137cc5ac2a85d701ce2991cab5dcd538',
    '0x7aa5237e0f999a9853a9cc8c56093220142ce48e',
    '0xd43f262536e916a4a807d27080092f190e25d774',
    '0xdd8422eed7fe5f85ea8058d273d3f5c17ef41d1c',

    '0xfa578b05fbd9e1e7c1e69d5add1113240d641bc2',
    '0x56c8b9ab7a9594f2d60427fcedbff6ab63c43281',
];

let MSRequired = 1;
// let freezingTime = 60 * 15;
let freezingTime = 0;

module.exports = function (deployer, network) {
    deployer.then(async () => { // eslint-disable-line promise/catch-or-return
        if (network === 'privateLive') {
            // 0) deploy `Gatekeeper` multisig
            await deployer.deploy(sidechainGateMS, MSOwners, MSRequired, { gasPrice: 0 });
            let multiSig = await sidechainGateMS.deployed();

            // 1) deploy SNM token
            await deployer.deploy(SNM, { gasPrice: 0 });
            let token = await SNM.deployed();

            // 2) deploy Gatekeper
            await deployer.deploy(Gatekeeper, token.address, freezingTime, { gasPrice: 0 });
            let gk = await Gatekeeper.deployed();

            // 3) transfer all tokens to Gatekeeper
            // await token.transfer(gk.address, 444 * 1e6 * 1e18, { gasPrice: 0 });
            await token.transfer(gk.address, 440 * 1e6 * 1e18, { gasPrice: 0 });
            await token.transfer('0xfa578b05fbd9e1e7c1e69d5add1113240d641bc2', 4 * 1e6 * 1e18, { gasPrice: 0 });

            // 3.1): add keeper with 100k limit for testing
            await gk.ChangeKeeperLimit('0x1f0dc2f125a2df9e37f32242cc3e34328f096b3c', 100000 * 1e18, { gasPrice: 0 });
            // local
            await gk.ChangeKeeperLimit('0xfa578b05fbd9e1e7c1e69d5add1113240d641bc2', 100000 * 1e18, { gasPrice: 0 }); // eslint-disable-line max-len
            // 172.18.196.12
            await gk.ChangeKeeperLimit('0x980469cf401238e6b1d333101a24cfad7736d708', 100000 * 1e18, { gasPrice: 0 }); // eslint-disable-line max-len

            // 4) transfer Gatekeeper ownership to `Gatekeeper` multisig
            await gk.transferOwnership(multiSig.address, { gasPrice: 0 });
        }
    });
};
