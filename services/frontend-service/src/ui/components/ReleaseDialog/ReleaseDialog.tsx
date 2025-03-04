/*This file is part of kuberpult.

Kuberpult is free software: you can redistribute it and/or modify
it under the terms of the Expat(MIT) License as published by
the Free Software Foundation.

Kuberpult is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
MIT License for more details.

You should have received a copy of the MIT License
along with kuberpult. If not, see <https://directory.fsf.org/wiki/License:Expat>.

Copyright 2023 freiheit.com*/
import { Dialog } from '@material-ui/core';
import classNames from 'classnames';
import React, { ReactElement, useCallback } from 'react';
import { Environment, Environment_Application, EnvironmentGroup, Lock, LockBehavior, Release } from '../../../api/api';
import {
    addAction,
    useCloseReleaseDialog,
    useEnvironmentGroups,
    useReleaseOptional,
    useReleaseOrThrow,
    useTeamFromApplication,
} from '../../utils/store';
import { Button } from '../button';
import { Close, Locks } from '../../../images';
import { EnvironmentChip } from '../chip/EnvironmentGroupChip';
import { FormattedDate } from '../FormattedDate/FormattedDate';
import { ArgoAppLink, ArgoTeamLink, DisplayManifestLink, DisplaySourceLink } from '../../utils/Links';
import { ReleaseVersion } from '../ReleaseVersion/ReleaseVersion';

export type ReleaseDialogProps = {
    className?: string;
    app: string;
    version: number;
};

export const AppLock: React.FC<{
    env: Environment;
    app: string;
    lock: Lock;
}> = ({ env, app, lock }) => {
    const deleteAppLock = useCallback(() => {
        addAction({
            action: {
                $case: 'deleteEnvironmentApplicationLock',
                deleteEnvironmentApplicationLock: { environment: env.name, application: app, lockId: lock.lockId },
            },
        });
    }, [app, env.name, lock.lockId]);
    return (
        <div
            title={'App Lock Message: "' + lock.message + '" | ID: "' + lock.lockId + '"  | Click to unlock. '}
            onClick={deleteAppLock}>
            <Button icon={<Locks className="env-card-app-lock" />} className={'button-lock'} />
        </div>
    );
};

export type EnvironmentListItemProps = {
    env: Environment;
    app: string;
    release: Release;
    queuedVersion: number;
    className?: string;
};

type CommitIdProps = {
    application: Environment_Application;
    app: string;
    env: Environment;
    otherRelease?: Release;
};

const DeployedVersion: React.FC<CommitIdProps> = ({ application, app, env, otherRelease }): ReactElement => {
    if (!application || !otherRelease) {
        return (
            <span>
                "{app}" has no version deployed on "{env.name}"
            </span>
        );
    }
    const firstLine = otherRelease.sourceMessage.split('\n')[0];
    return (
        <span>
            <ReleaseVersion release={otherRelease} />
            {firstLine}
        </span>
    );
};

export const EnvironmentListItem: React.FC<EnvironmentListItemProps> = ({
    env,
    app,
    release,
    queuedVersion,
    className,
}) => {
    const createAppLock = useCallback(() => {
        addAction({
            action: {
                $case: 'createEnvironmentApplicationLock',
                createEnvironmentApplicationLock: {
                    environment: env.name,
                    application: app,
                    lockId: '',
                    message: '',
                },
            },
        });
    }, [app, env.name]);
    const deployAndLockClick = useCallback(() => {
        if (release.version) {
            addAction({
                action: {
                    $case: 'deploy',
                    deploy: {
                        environment: env.name,
                        application: app,
                        version: release.version,
                        ignoreAllLocks: false,
                        lockBehavior: LockBehavior.Ignore,
                    },
                },
            });
            createAppLock();
        }
    }, [release.version, app, env.name, createAppLock]);

    const queueInfo =
        queuedVersion === 0 ? null : (
            <div
                className={classNames('env-card-data env-card-data-queue', className)}
                title={
                    'An attempt was made to deploy version ' +
                    queuedVersion +
                    ' either by a release train, or when a new version was created. However, there was a lock present at the time, so kuberpult did not deploy this version. '
                }>
                Version {queuedVersion} was not deployed, because of a lock.
            </div>
        );
    const otherRelease = useReleaseOptional(app, env);
    const application = env.applications[app];
    const getDeploymentMetadata = (): [String, JSX.Element] => {
        if (!application) {
            return ['', <></>];
        }
        if (!(application && application.version === release.version)) {
            return ['', <></>];
        }
        if (application.deploymentMetaData === null) {
            return ['', <></>];
        }
        const deployedBy = application.deploymentMetaData?.deployAuthor ?? 'unknown';
        const deployedUNIX = application.deploymentMetaData?.deployTime ?? '';
        if (deployedUNIX === '') {
            return ['Deployed by ' + deployedBy, <></>];
        }
        const deployedDate = new Date(+deployedUNIX * 1000);
        const returnString = 'Deployed by ' + deployedBy + ' ';
        const time = (
            <FormattedDate createdAt={deployedDate} className={classNames('release-dialog-createdAt', className)} />
        );

        return [returnString, time];
    };
    return (
        <li key={env.name} className={classNames('env-card', className)}>
            <div className="env-card-header">
                <EnvironmentChip
                    env={env}
                    app={app}
                    className={'release-environment'}
                    key={env.name}
                    groupNameOverride={undefined}
                    numberEnvsDeployed={undefined}
                    numberEnvsInGroup={undefined}
                />
                <div className={classNames('env-card-app-locks')}>
                    {Object.values(env.applications)
                        .filter((application) => application.name === app)
                        .map((app) => app.locks)
                        .map((locks) =>
                            Object.values(locks).map((lock) => (
                                <AppLock key={lock.lockId} env={env} app={app} lock={lock} />
                            ))
                        )}
                </div>
            </div>
            <div className="content-area">
                <div className="content-left">
                    <div
                        className={classNames('env-card-data', className)}
                        title={
                            'Shows the version that is currently deployed on ' +
                            env.name +
                            '. ' +
                            (release.undeployVersion ? undeployTooltipExplanation : '')
                        }>
                        <DeployedVersion app={app} env={env} application={application} otherRelease={otherRelease} />
                    </div>
                    {queueInfo}
                    <div className={classNames('env-card-data', className)}>
                        {getDeploymentMetadata().flatMap((metadata, i) => (
                            <div key={i}>{metadata}</div>
                        ))}
                    </div>
                </div>
                <div className="content-right">
                    <div className="env-card-buttons">
                        <Button
                            className="env-card-add-lock-btn"
                            label="Add lock"
                            onClick={createAppLock}
                            icon={<Locks className="icon" />}
                        />
                        <div
                            title={
                                'When doing manual deployments, it is usually best to also lock the app. If you omit the lock, an automatic release train or another person may deploy an unintended version. If you do not want a lock, you can remove it from the "planned actions".'
                            }>
                            <Button
                                disabled={application && application.version === release.version}
                                className={classNames('env-card-deploy-btn', 'mdc-button--unelevated')}
                                onClick={deployAndLockClick}
                                label="Deploy & Lock"
                            />
                        </div>
                    </div>
                </div>
            </div>
        </li>
    );
};

export const EnvironmentList: React.FC<{
    release: Release;
    app: string;
    version: number;
    className?: string;
}> = ({ release, app, version, className }) => {
    const allEnvGroups: EnvironmentGroup[] = useEnvironmentGroups();
    return (
        <div className="release-env-group-list">
            {allEnvGroups.map((envGroup) => (
                <ul className={classNames('release-env-list', className)} key={envGroup.environmentGroupName}>
                    {envGroup.environments.map((env) => (
                        <EnvironmentListItem
                            key={env.name}
                            env={env}
                            app={app}
                            release={release}
                            className={className}
                            queuedVersion={env.applications[app] ? env.applications[app].queuedVersion : 0}
                        />
                    ))}
                </ul>
            ))}
        </div>
    );
};

export const undeployTooltipExplanation =
    'This is the "undeploy" version. It is essentially an empty manifest. Deploying this means removing all kubernetes entities like deployments from the given environment. You must deploy this to all environments before kuberpult allows to delete the app entirely.';

export const ReleaseDialog: React.FC<ReleaseDialogProps> = (props) => {
    const { app, className, version } = props;
    // the ReleaseDialog is only opened when there is a release, so we can assume that it exists here:
    const release = useReleaseOrThrow(app, version);
    const team = useTeamFromApplication(app);
    const closeReleaseDialog = useCloseReleaseDialog();

    const dialog: JSX.Element | '' =
        app !== '' ? (
            <div>
                <Dialog
                    className={classNames('release-dialog', className)}
                    fullWidth={true}
                    maxWidth="md"
                    open={app !== ''}
                    onClose={closeReleaseDialog}>
                    <div className={classNames('release-dialog-app-bar', className)}>
                        <div className={classNames('release-dialog-app-bar-data')}>
                            <div className={classNames('release-dialog-message', className)}>
                                <span className={classNames('release-dialog-commitMessage', className)}>
                                    {release?.sourceMessage}
                                </span>
                            </div>
                            <div className="source">
                                <span>
                                    {'Created '}
                                    {release?.createdAt ? (
                                        <FormattedDate
                                            createdAt={release.createdAt}
                                            className={classNames('release-dialog-createdAt', className)}
                                        />
                                    ) : (
                                        'at an unknown date'
                                    )}
                                    {' by '}
                                    {release?.sourceAuthor ? release?.sourceAuthor : 'an unknown author'}{' '}
                                </span>
                                <span className="links">
                                    <DisplaySourceLink commitId={release.sourceCommitId} displayString={'Source'} />
                                    &nbsp;
                                    <DisplayManifestLink app={app} version={release.version} displayString="Manifest" />
                                </span>
                            </div>
                            <div className={classNames('release-dialog-app', className)}>
                                {'App: '}
                                <ArgoAppLink app={app} />
                                <ArgoTeamLink team={team} />
                            </div>
                        </div>
                        <Button
                            onClick={closeReleaseDialog}
                            className={classNames('release-dialog-close', className)}
                            icon={<Close />}
                        />
                    </div>
                    <EnvironmentList app={app} className={className} release={release} version={version} />
                </Dialog>
            </div>
        ) : (
            ''
        );

    return <div>{dialog}</div>;
};
